// remotemeta.go wraps a local MetaStore with mesh replication so /cer/fs metadata
// (path→Manifest) is visible on every connected peer: a write on node B pushes
// the Manifest to known peers; a read or list on node A pulls from peers on a
// local miss. Shard bytes still scatter via RemoteScatterShardStore (shardstore.go);
// this file handles the metadata half of multi-node /cer/fs.
package system

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/dfs"
	"github.com/hash066/cerberus/daemon/mesh"

	contract "github.com/hash066/cerberus/contract/go"
)

const (
	metaReplicateTimeout = 15 * time.Second
	metaFetchTimeout     = 20 * time.Second
	metaListFanoutCap    = 8
)

// metaFabric is the mesh slice ReplicatedMetaStore needs for metadata RPC.
type metaFabric interface {
	Peers() []contract.PeerInfo
	RequestPutMeta(ctx context.Context, peer contract.PeerID, path string, manifestJSON []byte, capEnvelope []byte, issuer contract.PeerID) error
	RequestGetMeta(ctx context.Context, peer contract.PeerID, path string, capEnvelope []byte, issuer contract.PeerID) ([]byte, error)
	RequestListMeta(ctx context.Context, peer contract.PeerID, capEnvelope []byte, issuer contract.PeerID) ([]string, error)
}

var _ metaFabric = (*mesh.Fabric)(nil)

// ReplicatedMetaStore is a MetaStore that keeps a local copy and replicates
// path→Manifest records across the mesh.
type ReplicatedMetaStore struct {
	local    MetaStore
	fabric   metaFabric
	signer   *auth.SignedCap
	site     string
	issuerID contract.PeerID
}

// NewReplicatedMetaStore wraps local with mesh push-on-write / pull-on-miss.
// A nil signer disables remote replication (pure-local behaviour).
func NewReplicatedMetaStore(local MetaStore, fabric metaFabric, signer *auth.SignedCap, site string) *ReplicatedMetaStore {
	r := &ReplicatedMetaStore{local: local, fabric: fabric, signer: signer, site: site}
	if signer != nil {
		if id, err := signer.IssuerPeerID(); err == nil {
			r.issuerID = id
		} else {
			r.signer = nil
		}
	}
	return r
}

func (r *ReplicatedMetaStore) mintMetaCap(right contract.Right) ([]byte, bool) {
	if r.signer == nil {
		return nil, false
	}
	g, err := auth.NewGrant(mesh.MeshMetaResource(r.site), []contract.Right{right}, nil, time.Hour)
	if err != nil {
		return nil, false
	}
	env, err := r.signer.Issue(g)
	if err != nil {
		return nil, false
	}
	return env, true
}

// Put records manifest locally and replicates to every known peer (best-effort,
// bounded wait so a write does not return before peers have the metadata).
func (r *ReplicatedMetaStore) Put(path string, man dfs.Manifest) error {
	if err := r.local.Put(path, man); err != nil {
		return err
	}
	r.replicatePut(path, man)
	return nil
}

func (r *ReplicatedMetaStore) replicatePut(path string, man dfs.Manifest) {
	env, ok := r.mintMetaCap(contract.RightWrite)
	if !ok {
		return
	}
	raw, err := json.Marshal(man)
	if err != nil {
		return
	}
	peers := r.fabric.Peers()
	if len(peers) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, p := range peers {
		wg.Add(1)
		go func(peer contract.PeerID) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), metaReplicateTimeout)
			defer cancel()
			_ = r.fabric.RequestPutMeta(ctx, peer, path, raw, env, r.issuerID)
		}(p.ID)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(metaReplicateTimeout):
	}
}

// Get returns the manifest for path, fetching from peers on a local miss.
func (r *ReplicatedMetaStore) Get(path string) (dfs.Manifest, bool) {
	if man, ok := r.local.Get(path); ok {
		return man, true
	}
	raw, ok := r.fetchRemote(path)
	if !ok {
		return dfs.Manifest{}, false
	}
	var man dfs.Manifest
	if err := json.Unmarshal(raw, &man); err != nil {
		return dfs.Manifest{}, false
	}
	// Cache locally so subsequent reads and dfs.Get shard fetches are fast.
	_ = r.local.Put(path, man)
	return man, true
}

func (r *ReplicatedMetaStore) fetchRemote(path string) ([]byte, bool) {
	env, ok := r.mintMetaCap(contract.RightRead)
	if !ok {
		return nil, false
	}
	peers := r.fabric.Peers()
	if len(peers) == 0 {
		return nil, false
	}
	if len(peers) > metaListFanoutCap {
		peers = peers[:metaListFanoutCap]
	}

	ctx, cancel := context.WithTimeout(context.Background(), metaFetchTimeout)
	defer cancel()

	type result struct {
		data []byte
		ok   bool
	}
	results := make(chan result, len(peers))
	for _, p := range peers {
		go func(peer contract.PeerID) {
			raw, err := r.fabric.RequestGetMeta(ctx, peer, path, env, r.issuerID)
			select {
			case results <- result{data: raw, ok: err == nil && len(raw) > 0}:
			case <-ctx.Done():
			}
		}(p.ID)
	}

	for i := 0; i < len(peers); i++ {
		select {
		case res := <-results:
			if res.ok {
				return res.data, true
			}
		case <-ctx.Done():
			return nil, false
		}
	}
	return nil, false
}

// List merges local paths with paths reported by known peers.
func (r *ReplicatedMetaStore) List() ([]string, error) {
	local, err := r.local.List()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, p := range local {
		seen[p] = true
	}

	env, ok := r.mintMetaCap(contract.RightRead)
	if ok {
		peers := r.fabric.Peers()
		if len(peers) > metaListFanoutCap {
			peers = peers[:metaListFanoutCap]
		}
		ctx, cancel := context.WithTimeout(context.Background(), metaFetchTimeout)
		var wg sync.WaitGroup
		var mu sync.Mutex
		for _, p := range peers {
			wg.Add(1)
			go func(peer contract.PeerID) {
				defer wg.Done()
				paths, err := r.fabric.RequestListMeta(ctx, peer, env, r.issuerID)
				if err != nil {
					return
				}
				mu.Lock()
				for _, path := range paths {
					seen[path] = true
				}
				mu.Unlock()
			}(p.ID)
		}
		wg.Wait()
		cancel()
	}

	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

var _ MetaStore = (*ReplicatedMetaStore)(nil)

// LocalMetaServer adapts a MetaStore to mesh.MetaServer for ServeMeta.
type LocalMetaServer struct {
	local MetaStore
}

// NewLocalMetaServer wraps local for remote metadata RPC serving.
func NewLocalMetaServer(local MetaStore) *LocalMetaServer {
	return &LocalMetaServer{local: local}
}

func (l *LocalMetaServer) PutMeta(path string, manifestJSON []byte) error {
	var man dfs.Manifest
	if err := json.Unmarshal(manifestJSON, &man); err != nil {
		return fmt.Errorf("meta: bad manifest json: %w", err)
	}
	return l.local.Put(path, man)
}

func (l *LocalMetaServer) GetMeta(path string) ([]byte, bool) {
	man, ok := l.local.Get(path)
	if !ok {
		return nil, false
	}
	raw, err := json.Marshal(man)
	if err != nil {
		return nil, false
	}
	return raw, true
}

func (l *LocalMetaServer) ListMeta() ([]string, error) {
	return l.local.List()
}

var _ mesh.MetaServer = (*LocalMetaServer)(nil)
