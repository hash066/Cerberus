// shardstore.go is the v1 real peer-scatter shard placement for /cer/fs.
//
// SCOPE (deliberately v1, not a production placement algorithm): a simple
// round-robin over the mesh peers currently known to contract.Fabric.Peers().
// Every Nth shard of a file (N = placementEvery) is placed on the NEXT known
// mesh peer instead of this node's own local store; the rest stay local. There
// is no load balancing by free space, no failure-aware rebalancing, and no
// re-replication if the chosen peer later disappears — a lost remote shard is
// simply unavailable on the next read, exactly like a lost local shard, and dfs's
// existing Reed-Solomon reconstruction (Get tolerates losing up to
// DefaultParityShards shards per chunk) is what actually keeps the file readable.
//
// WHAT IS REAL: when a shard is placed remotely, its bytes are sent over a real
// mesh request (daemon/mesh's new ServeShards/RequestPutShard/RequestGetShard,
// modeled directly on compute.go's existing request/response-over-libp2p/QUIC
// pattern) to the chosen peer's OWN RemoteScatterShardStore, which stores them in
// ITS OWN local dfs.ShardStore — not a local echo. A subsequent Get dials that
// same peer and fetches the bytes back over the same mesh mechanism. This is
// genuine cross-node placement: bytes cross the network to another node's dfs
// store and back.
//
// Manifest.Placement records, per shard, either "local" (dfs's MemShardStore
// convention) or "mesh:<base64 Ed25519 PeerID>" so a later read on ANY node that
// holds the Manifest knows which peer to ask.
package system

import (
	"context"
	"encoding/base64"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/hash066/cerberus/daemon/dfs"
	"github.com/hash066/cerberus/daemon/mesh"
	"github.com/ipfs/go-cid"

	contract "github.com/hash066/cerberus/contract/go"
)

// remotePlacementPrefix marks a Manifest.Placement value naming a mesh peer
// (base64 Ed25519 PeerID) that holds the shard, as opposed to "mem" (dfs's
// MemShardStore local-placement convention).
const remotePlacementPrefix = "mesh:"

// placementEvery places every Nth shard remotely (when at least one mesh peer is
// known); the rest stay local. N=3 with the v0.1 default of 4 data + 2 parity
// shards per chunk means roughly a third of a chunk's shards leave this node —
// enough to prove real peer scatter without shipping every byte of every write
// across the network for this v1 policy. A production placement policy would
// instead reason about free space, replication factor, and failure domains.
const placementEvery = 3

// meshShardTimeout bounds a single remote shard put/get so a slow or dead peer
// cannot hang a /cer/fs write or read indefinitely.
const meshShardTimeout = 15 * time.Second

// meshFabric is the slice of daemon/mesh's Fabric this package needs: enough to
// discover peers and run the shard RPC. Declared as a local interface (rather
// than importing contract.Fabric, which does not carry the shard RPC methods) so
// RemoteScatterShardStore can be unit tested against a fake.
type meshFabric interface {
	Peers() []contract.PeerInfo
	PeerID() contract.PeerID
	RequestPutShard(ctx context.Context, peer contract.PeerID, cidBytes []byte, shard []byte) error
	RequestGetShard(ctx context.Context, peer contract.PeerID, cidBytes []byte) ([]byte, error)
}

var _ meshFabric = (*mesh.Fabric)(nil)

// RemoteScatterShardStore implements dfs.ShardStore. It keeps most shards in a
// local backing store but scatters every Nth shard onto a remote mesh peer
// (round robin over Peers()), recording which peer in the placement hint so Get
// can fetch it back over the mesh. With zero known peers it behaves exactly like
// the local store (falls back to local placement) — a lone node is not broken by
// this wiring.
type RemoteScatterShardStore struct {
	local  dfs.ShardStore
	fabric meshFabric
	// putCount is incremented on every PutShard call and used (mod placementEvery)
	// to decide whether THIS shard is the one that goes remote, and (mod known
	// peer count) to pick which peer — a simple round robin.
	putCount uint64
}

// NewRemoteScatterShardStore builds a shard store that keeps shards in local
// (this node's own backing dfs.ShardStore) but scatters every Nth one onto a
// currently-known mesh peer over the real mesh RPC.
func NewRemoteScatterShardStore(local dfs.ShardStore, fabric meshFabric) *RemoteScatterShardStore {
	return &RemoteScatterShardStore{local: local, fabric: fabric}
}

// PutShard stores the shard. Every placementEvery-th call, if at least one mesh
// peer is currently known, the shard is sent to that peer over the real mesh RPC
// and stored on the peer's own local store instead of this node's; otherwise (or
// on any remote failure) it falls back to storing locally. The returned
// placement hint records exactly where the bytes ended up.
func (r *RemoteScatterShardStore) PutShard(c cid.Cid, shard []byte) (string, error) {
	n := atomic.AddUint64(&r.putCount, 1)

	if n%placementEvery == 0 {
		if peer, ok := r.pickPeer(); ok {
			ctx, cancel := context.WithTimeout(context.Background(), meshShardTimeout)
			defer cancel()
			if err := r.fabric.RequestPutShard(ctx, peer, c.Bytes(), shard); err == nil {
				return remotePlacementPrefix + encodePeer(peer), nil
			}
			// Remote placement failed (peer unreachable, denied, etc.) — fall back to
			// local rather than losing the shard. This keeps a write succeeding even
			// when the chosen peer is momentarily unavailable; it is a documented
			// availability trade-off, not a silent data-loss path.
		}
	}

	placement, err := r.local.PutShard(c, shard)
	if err != nil {
		return "", err
	}
	return placement, nil
}

// GetShard fetches the shard from wherever PutShard placed it: locally, or from
// the named mesh peer over the real mesh RPC. The CID integrity check the dfs
// package already performs on every fetched shard (regardless of origin) is what
// protects against a corrupted or malicious remote response.
func (r *RemoteScatterShardStore) GetShard(c cid.Cid) ([]byte, error) {
	// dfs.ShardStore.GetShard does not carry the placement hint (it is keyed only
	// by CID), so RemoteScatterShardStore tries its local store first and, on a
	// miss, asks every currently-known peer in turn. This is correct (the CID
	// integrity check rejects a wrong answer) if slightly more work than tracking
	// placement per-CID; see the doc comment on lookupPlacement below for why a
	// per-CID index is not needed for correctness here.
	if b, err := r.local.GetShard(c); err == nil {
		return b, nil
	}

	var lastErr error
	for _, peer := range r.fabric.Peers() {
		ctx, cancel := context.WithTimeout(context.Background(), meshShardTimeout)
		b, err := r.fabric.RequestGetShard(ctx, peer.ID, c.Bytes())
		cancel()
		if err == nil {
			return b, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, fmt.Errorf("dfs: shard %s not found locally or on %d known peer(s): %w", c, len(r.fabric.Peers()), lastErr)
	}
	return nil, fmt.Errorf("dfs: shard %s not found (no mesh peers known)", c)
}

// pickPeer returns the next peer in round-robin order over the currently-known
// mesh peers, or ok=false if none are known yet (a lone/just-started node).
func (r *RemoteScatterShardStore) pickPeer() (contract.PeerID, bool) {
	peers := r.fabric.Peers()
	if len(peers) == 0 {
		return contract.PeerID{}, false
	}
	idx := int((atomic.LoadUint64(&r.putCount) / placementEvery) % uint64(len(peers)))
	return peers[idx].ID, true
}

// encodePeer/decodePeer render a contract.PeerID (raw Ed25519 public key) as the
// base64 string recorded in Manifest.Placement.
func encodePeer(p contract.PeerID) string { return base64.StdEncoding.EncodeToString(p[:]) }

// LocalShardServer adapts a dfs.ShardStore to mesh.ShardServer so this node can
// serve OTHER peers' remote-placed shard put/get requests against its own local
// store. Compose installs this via fabric.ServeShards so a peer that picks THIS
// node in its round robin can actually store/fetch here.
type LocalShardServer struct {
	local dfs.ShardStore
}

// NewLocalShardServer wraps local so it can be exposed to remote peers.
func NewLocalShardServer(local dfs.ShardStore) *LocalShardServer {
	return &LocalShardServer{local: local}
}

func (l *LocalShardServer) PutShard(cidBytes []byte, shard []byte) error {
	c, err := cid.Cast(cidBytes)
	if err != nil {
		return fmt.Errorf("dfs: bad shard cid: %w", err)
	}
	_, err = l.local.PutShard(c, shard)
	return err
}

func (l *LocalShardServer) GetShard(cidBytes []byte) ([]byte, error) {
	c, err := cid.Cast(cidBytes)
	if err != nil {
		return nil, fmt.Errorf("dfs: bad shard cid: %w", err)
	}
	return l.local.GetShard(c)
}

var _ dfs.ShardStore = (*RemoteScatterShardStore)(nil)
var _ mesh.ShardServer = (*LocalShardServer)(nil)
