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
// mesh request (daemon/mesh's ServeShards/RequestPutShard/RequestGetShard,
// modeled directly on compute.go's existing request/response-over-libp2p/QUIC
// pattern) to the chosen peer's OWN RemoteScatterShardStore, which stores them in
// ITS OWN local dfs.ShardStore — not a local echo. A subsequent Get dials that
// same peer and fetches the bytes back over the same mesh mechanism. This is
// genuine cross-node placement: bytes cross the network to another node's dfs
// store and back.
//
// CAPABILITY GATE: every RequestPutShard/RequestGetShard call now mints (or
// reuses, memoized) a signed capability envelope naming
// mesh.MeshShardResource(site) with RightWrite (put) or RightRead (get) and
// attaches it to the wire request — RemoteScatterShardStore is the client that
// makes the shard RPC's capability gate (daemon/mesh/shard.go) meaningful, not
// just possible. Without an *auth.SignedCap issuer configured (signer==nil),
// RemoteScatterShardStore falls back to purely-local placement (never presents
// an unauthenticated request on the wire) — see WithSignedCapIssuer.
//
// Manifest.Placement records, per shard, either "local" (dfs's MemShardStore
// convention) or "mesh:<base64 Ed25519 PeerID>" so a later read on ANY node that
// holds the Manifest knows which peer to ask.
package system

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hash066/cerberus/daemon/auth"
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

// getShardFanoutCap bounds how many currently-known peers a GetShard local-miss
// fallback queries concurrently. A mesh with many more peers than this still
// only probes the first getShardFanoutCap of them per attempt — enough to make a
// genuine cross-node hit likely without turning "ask everyone" into an O(N)
// traffic-analysis broadcast of which CIDs this node wants.
const getShardFanoutCap = 8

// getShardOverallTimeout bounds the ENTIRE fan-out across all queried peers, not
// each peer individually — replacing the old sequential per-peer timeout (which
// made a genuine miss take up to N*meshShardTimeout on an N-peer mesh) with a
// single deadline so a miss fails predictably fast regardless of mesh size.
const getShardOverallTimeout = 20 * time.Second

// meshFabric is the slice of daemon/mesh's Fabric this package needs: enough to
// discover peers and run the shard RPC. Declared as a local interface (rather
// than importing contract.Fabric, which does not carry the shard RPC methods) so
// RemoteScatterShardStore can be unit tested against a fake.
type meshFabric interface {
	Peers() []contract.PeerInfo
	PeerID() contract.PeerID
	RequestPutShard(ctx context.Context, peer contract.PeerID, cidBytes []byte, shard []byte, capEnvelope []byte, issuer contract.PeerID) error
	RequestGetShard(ctx context.Context, peer contract.PeerID, cidBytes []byte, capEnvelope []byte, issuer contract.PeerID) ([]byte, error)
}

var _ meshFabric = (*mesh.Fabric)(nil)

// NewShardCapSigner builds the *auth.SignedCap issuer RemoteScatterShardStore
// uses to mint shard-placement capabilities, keyed off the SAME Ed25519 keypair
// as the mesh identity (fabric.Identity()) rather than a separate issuer key.
// This is required, not a convenience: daemon/mesh's ServeShards binds a shard
// request's claimed Issuer to the PeerID the QUIC/TLS handshake actually
// authenticated for the stream (see shard.go's "Issuer trust model" doc
// comment), so a signer whose IssuerPeerID() differs from this node's own
// fabric.PeerID() would mint envelopes every peer's gate rejects. Returns
// (nil, error) if identity is not a valid Ed25519 seed-bearing key (should not
// happen for a real *mesh.Fabric — see mesh.Fabric.Identity's doc comment).
func NewShardCapSigner(identity ed25519.PrivateKey) (*auth.SignedCap, error) {
	if len(identity) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("system: mesh identity is not a usable Ed25519 private key (len=%d)", len(identity))
	}
	ks, err := auth.NewMemoryKeyStore(identity.Seed())
	if err != nil {
		return nil, fmt.Errorf("system: build shard-cap keystore from mesh identity: %w", err)
	}
	return auth.NewSignedCap(ks), nil
}

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

	// signer/site/issuerID mint the signed capability envelope every remote shard
	// RPC presents on the wire (see mesh.MeshShardResource). A nil signer means
	// this store never presents a remote request at all — PutShard/GetShard
	// behave as pure-local (no unauthenticated wire call is ever made).
	signer   *auth.SignedCap
	site     string
	issuerID contract.PeerID
}

// NewRemoteScatterShardStore builds a shard store that keeps shards in local
// (this node's own backing dfs.ShardStore) but scatters every Nth one onto a
// currently-known mesh peer over the real mesh RPC.
//
// signer mints the signed capability envelope this store attaches to every
// remote PutShard/GetShard wire call (see mesh.MeshShardResource(site)); site
// must match the mesh.Config.Site the local ServeShards gate was configured
// with (Compose passes the same site value to both). A nil signer disables
// remote placement entirely (PutShard/GetShard fall back to purely local) rather
// than ever sending an unauthenticated request — fail closed on missing
// configuration, not fail open.
func NewRemoteScatterShardStore(local dfs.ShardStore, fabric meshFabric, signer *auth.SignedCap, site string) *RemoteScatterShardStore {
	r := &RemoteScatterShardStore{local: local, fabric: fabric, signer: signer, site: site}
	if signer != nil {
		if id, err := signer.IssuerPeerID(); err == nil {
			r.issuerID = id
		} else {
			// Cannot recover our own issuer PeerID — treat as unconfigured so we never
			// mint a cap under a zero-value Issuer (which would just fail Verify on the
			// remote end anyway, but this makes the local intent explicit).
			r.signer = nil
		}
	}
	return r
}

// mintShardCap mints a fresh signed capability naming mesh.MeshShardResource(r.site)
// with the given right. Returns (nil, false) when no signer is configured — the
// caller must treat that as "cannot make a remote request", never as "make an
// unauthenticated one".
func (r *RemoteScatterShardStore) mintShardCap(right contract.Right) ([]byte, bool) {
	if r.signer == nil {
		return nil, false
	}
	g, err := auth.NewGrant(mesh.MeshShardResource(r.site), []contract.Right{right}, nil, time.Hour)
	if err != nil {
		return nil, false
	}
	env, err := r.signer.Issue(g)
	if err != nil {
		return nil, false
	}
	return env, true
}

// PutShard stores the shard. Every placementEvery-th call, if at least one mesh
// peer is currently known AND a signed-cap issuer is configured, the shard is
// sent to that peer over the real mesh RPC (carrying a freshly minted RightWrite
// capability) and stored on the peer's own local store instead of this node's;
// otherwise (or on any remote failure) it falls back to storing locally. The
// returned placement hint records exactly where the bytes ended up.
func (r *RemoteScatterShardStore) PutShard(c cid.Cid, shard []byte) (string, error) {
	n := atomic.AddUint64(&r.putCount, 1)

	if n%placementEvery == 0 {
		if peer, ok := r.pickPeer(); ok {
			if env, ok := r.mintShardCap(contract.RightWrite); ok {
				ctx, cancel := context.WithTimeout(context.Background(), meshShardTimeout)
				err := r.fabric.RequestPutShard(ctx, peer, c.Bytes(), shard, env, r.issuerID)
				cancel()
				if err == nil {
					return remotePlacementPrefix + encodePeer(peer), nil
				}
				// Remote placement failed (peer unreachable, denied, etc.) — fall back to
				// local rather than losing the shard. This keeps a write succeeding even
				// when the chosen peer is momentarily unavailable; it is a documented
				// availability trade-off, not a silent data-loss path.
			}
		}
	}

	placement, err := r.local.PutShard(c, shard)
	if err != nil {
		return "", err
	}
	_ = placement
	// Record this node's own PeerID so any reader holding the Manifest can dial
	// the correct peer directly (see GetPlacedShard) instead of relying on a
	// blind fan-out that may miss under load.
	return remotePlacementPrefix + encodePeer(r.fabric.PeerID()), nil
}

// getShardResult is one fan-out worker's outcome, for the first-success
// collector in GetShard.
type getShardResult struct {
	data []byte
	err  error
	peer contract.PeerID
}

// GetPlacedShard fetches a shard using the Manifest placement hint recorded at
// PutShard time (always "mesh:<owner PeerID>" for this store).
func (r *RemoteScatterShardStore) GetPlacedShard(c cid.Cid, placement string) ([]byte, error) {
	if strings.HasPrefix(placement, remotePlacementPrefix) {
		peer, err := decodePeer(strings.TrimPrefix(placement, remotePlacementPrefix))
		if err != nil {
			return nil, err
		}
		if peer == r.fabric.PeerID() {
			return r.local.GetShard(c)
		}
		env, ok := r.mintShardCap(contract.RightRead)
		if !ok {
			return nil, fmt.Errorf("dfs: shard %s on %x: no signed-cap issuer configured", c, peer[:8])
		}
		ctx, cancel := context.WithTimeout(context.Background(), meshShardTimeout)
		defer cancel()
		return r.fabric.RequestGetShard(ctx, peer, c.Bytes(), env, r.issuerID)
	}
	return r.GetShard(c)
}

// GetShard fetches the shard from wherever PutShard placed it: locally, or from
// a currently-known mesh peer over the real mesh RPC. The CID integrity check
// the dfs package already performs on every fetched shard (regardless of
// origin) is what protects against a corrupted or malicious remote response.
//
// On a local miss, GetShard fans out to up to getShardFanoutCap known peers
// CONCURRENTLY (not sequentially) and returns as soon as one succeeds, bounding
// the WHOLE fan-out by getShardOverallTimeout rather than timing out each peer
// individually — a genuine miss on an N-peer mesh now fails in at most
// getShardOverallTimeout, not N*meshShardTimeout, and in-flight requests to the
// remaining peers are cancelled the moment one peer answers instead of
// broadcasting the wanted CID to every peer in turn regardless of an early hit.
func (r *RemoteScatterShardStore) GetShard(c cid.Cid) ([]byte, error) {
	// dfs.ShardStore.GetShard does not carry the placement hint (it is keyed only
	// by CID), so RemoteScatterShardStore tries its local store first and, on a
	// miss, asks known peers. This is correct (the CID integrity check rejects a
	// wrong answer) if slightly more work than tracking placement per-CID.
	if b, err := r.local.GetShard(c); err == nil {
		return b, nil
	}

	env, ok := r.mintShardCap(contract.RightRead)
	if !ok {
		return nil, fmt.Errorf("dfs: shard %s not found locally and no signed-cap issuer is configured for a remote fetch", c)
	}

	peers := r.fabric.Peers()
	if len(peers) == 0 {
		return nil, fmt.Errorf("dfs: shard %s not found (no mesh peers known)", c)
	}
	if len(peers) > getShardFanoutCap {
		peers = peers[:getShardFanoutCap]
	}

	ctx, cancel := context.WithTimeout(context.Background(), getShardOverallTimeout)
	defer cancel()

	results := make(chan getShardResult, len(peers))
	for _, p := range peers {
		go func(peer contract.PeerID) {
			b, err := r.fabric.RequestGetShard(ctx, peer, c.Bytes(), env, r.issuerID)
			select {
			case results <- getShardResult{data: b, err: err, peer: peer}:
			case <-ctx.Done():
			}
		}(p.ID)
	}

	var lastErr error
	for i := 0; i < len(peers); i++ {
		select {
		case res := <-results:
			if res.err == nil {
				return res.data, nil
			}
			lastErr = res.err
		case <-ctx.Done():
			if lastErr != nil {
				return nil, fmt.Errorf("dfs: shard %s not found locally or on %d queried peer(s) within %s: %w", c, len(peers), getShardOverallTimeout, lastErr)
			}
			return nil, fmt.Errorf("dfs: shard %s not found locally or on %d queried peer(s) within %s: %w", c, len(peers), getShardOverallTimeout, ctx.Err())
		}
	}
	if lastErr != nil {
		return nil, fmt.Errorf("dfs: shard %s not found locally or on %d queried peer(s): %w", c, len(peers), lastErr)
	}
	return nil, fmt.Errorf("dfs: shard %s not found on %d queried peer(s)", c, len(peers))
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

func decodePeer(b64 string) (contract.PeerID, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return contract.PeerID{}, fmt.Errorf("decode peer id: %w", err)
	}
	if len(raw) != len(contract.PeerID{}) {
		return contract.PeerID{}, fmt.Errorf("decode peer id: bad length %d", len(raw))
	}
	var p contract.PeerID
	copy(p[:], raw)
	return p, nil
}

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
var _ dfs.PlacedShardStore = (*RemoteScatterShardStore)(nil)
var _ mesh.ShardServer = (*LocalShardServer)(nil)
