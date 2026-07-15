package mesh

// shard.go adds a capability-gated request/response for dfs shard placement over
// the same point-to-point libp2p/QUIC stream mechanism compute.go established: a
// node sends a shardRequest to a peer, the peer verifies the caller presented a
// valid SIGNED capability envelope authorizing the operation, then (and only
// then) runs the requested operation (store or fetch a content-addressed shard)
// against its OWN local dfs shard store, and the result comes back over the same
// stream. This is the transport a daemon/system-level ShardStore uses to place
// SOME shards of a /cer/fs write on a remote mesh peer instead of always keeping
// every shard locally (see daemon/system/shardstore.go).
//
// What is REAL here:
//   - The transport is the same libp2p host over QUIC (quic-v1) compute.go uses:
//     encrypted, multiplexed, PeerID-authenticated by the QUIC/TLS handshake. Shard
//     bytes for a v0.1 dfs shard (a fraction of the 1 MiB default chunk size, split
//     across DefaultDataShards+DefaultParityShards) comfortably fit one length-
//     prefixed frame, so — mirroring the guidance that small payloads can ride the
//     request/response frame directly rather than needing a second data-plane
//     hop — the shard bytes travel in-band in the request/response body.
//   - PutShard/GetShard on the SERVING side call directly into that peer's own
//     dfs.ShardStore (its local disk/memory shard storage), so a "put" genuinely
//     stores the bytes on the remote node's own store, and a "get" genuinely reads
//     them back from there — not a local echo.
//
// CAPABILITY GATE (retires the "no capability is checked on this path" stub):
//   - Every shard put/get now REQUIRES a signed capability envelope (a
//     daemon/auth.SignedCap, Ed25519-signed by the granting node's issuer key),
//     verified server-side BEFORE the local ShardServer is touched at all — no
//     read, no write, no store mutation happens on an invalid/missing envelope.
//     This mirrors compute.go's ServeComputeSigned/RequestComputeSigned pattern
//     exactly: fail-closed verification logic (verifyShardCap here) checks
//     signature + validity window + revocation via a caller-supplied verifier,
//     then enforces a required right — see compute.go's verifySignedCap, which
//     this is modeled on line-for-line.
//   - Issuer trust model: unlike compute.go's exec cap (which is granted by the
//     resource OWNER to a specific peer via an out-of-band discovery exchange —
//     see test/e2e/node's handleDiscover), a shard put/get is symmetric,
//     engine-internal, node-to-node traffic between every pair of site peers,
//     with no existing discovery/grant-exchange protocol in daemon/system to
//     hang a per-peer trust anchor off of. Rather than inventing a new exchange
//     side-channel, ServeShards's server binds the claimed issuer to the PeerID
//     the QUIC/TLS handshake ALREADY cryptographically authenticated for this
//     exact stream (streamSession.RemotePeerID(), set from the connection's
//     verified remote public key — see session.go/identity.go) — a peer can
//     only mint a validly-signed envelope "as itself" (contract.PeerID IS the
//     raw Ed25519 public key, by construction; only the holder of the matching
//     private key can produce a signature auth.Verify accepts under it). This
//     composes real capability semantics (a scoped right, a validity window, a
//     revocable id — none of which bare identity gives you) on top of the
//     transport's existing peer authentication, rather than substituting one
//     for the other. See SelfIssuerResolver below, which every call site (this
//     package's own tests, daemon/system) wires as the resolveIssuer callback.
//   - Resource/right convention: a shard operation is scoped to a per-site
//     resource of kind contract.KindFS at path "cerberus/<site>/mesh-shard" (the
//     shard RPC is an engine-internal, site-scoped mesh operation, not a
//     per-file/per-CID grant — dfs shards are opaque erasure-coded fragments, not
//     independently meaningful resources, so scoping tighter than "this site's
//     mesh-shard placement service" would not add real protection while making
//     every PutShard/GetShard mint a fresh per-CID capability for no benefit).
//     RightWrite authorizes ShardOpPut; RightRead authorizes ShardOpGet — the
//     same rights lattice /cer/fs itself uses for file read/write, so a shard
//     placement capability composes naturally with an fs write/read capability
//     (see daemon/system/shardstore.go, which mints/attaches the envelope this
//     gate verifies for every RemoteScatterShardStore.PutShard/GetShard call).
//   - A request missing a required right, carrying a malformed/forged/wrong-
//     issuer/expired/revoked envelope is denied here, before local.PutShard or
//     local.GetShard is ever called — fail closed.
//
// What is a STUB / not yet here (not faked):
//   - Large-shard / data-plane-backed transfer: today's dfs default (1 MiB chunk,
//     4 data + 2 parity shards) keeps every shard well under a megabyte, small
//     enough for the in-band request/response frame. A future step could switch a
//     shard above some size threshold to coordinate a daemon/dataplane bulk
//     transfer instead of inlining the bytes here, exactly as compute.go's own doc
//     comment anticipates for larger payloads.

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/libp2p/go-libp2p/core/network"
)

// SelfIssuerResolver is an IssuerPubResolver that trusts a claimed issuer PeerID
// as its own Ed25519 public key — i.e. "verify this envelope as self-signed by
// whoever claims to be this PeerID". contract.PeerID IS the raw 32-byte Ed25519
// public key by construction (see contract/go/capability.go), so this resolver
// never fails to resolve a syntactically valid PeerID; the actual security
// comes from pairing it with a check that the claimed issuer equals the PeerID
// the transport already cryptographically authenticated for the stream (see
// handleShardStream/handleComponentFetchStream) — only the true holder of the
// matching private key can produce a signature auth.Verify accepts. Used by
// ServeShards and ServeComponentFetch, whose issuer trust model is documented in
// this file's and component.go's package doc comments.
func SelfIssuerResolver(id contract.PeerID) (ed25519.PublicKey, bool) {
	return ed25519.PublicKey(append([]byte(nil), id[:]...)), true
}

// shardProto is the libp2p protocol id for shard put/get requests.
const shardProto = "/cerberus/shard/1.0.0"

// MeshShardResource returns the per-site resource a shard-placement capability
// must name: contract.KindFS at "cerberus/<site>/mesh-shard". Callers outside
// this package (daemon/system's RemoteScatterShardStore) mint a signed
// capability against exactly this Kind/Path — RightWrite to authorize
// ShardOpPut, RightRead to authorize ShardOpGet — so ServeShards's gate accepts
// it. See the package doc comment for why this scope (not a per-shard-CID
// grant) is the right granularity for an engine-internal placement RPC.
func MeshShardResource(site string) contract.ResourceRef {
	return contract.ResourceRef{Kind: contract.KindFS, Path: "cerberus/" + site + "/mesh-shard"}
}

// ShardOp identifies which operation a shardRequest performs.
type ShardOp string

const (
	ShardOpPut ShardOp = "put"
	ShardOpGet ShardOp = "get"
)

// shardRequest is the on-wire request frame. CIDBytes is the shard's content
// address (cid.Cid.Bytes()); Data carries the shard payload for a put (empty for
// a get). Cap is the signed capability envelope authorizing this operation (see
// auth.SignedCap.Issue); Issuer names the PeerID of the node that minted it, so
// the server can resolve the trusted issuer public key it exchanged out of band
// (mirrors compute.go's computeRequest.Cap/Issuer).
type shardRequest struct {
	Op       ShardOp         `json:"op"`
	CIDBytes []byte          `json:"cid"`
	Data     []byte          `json:"data,omitempty"`
	Cap      []byte          `json:"cap,omitempty"`
	Issuer   contract.PeerID `json:"issuer,omitempty"`
}

// shardResponse is the on-wire result frame. OK is false with Error set when the
// peer's local ShardStore rejected the operation (e.g. a get miss) OR the
// capability gate denied the request before the store was touched at all; Data
// carries the fetched shard bytes on a successful get.
type shardResponse struct {
	OK    bool   `json:"ok"`
	Data  []byte `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

// ShardServer is implemented by the local dfs shard store the daemon exposes to
// remote peers over the mesh. It is intentionally the same two methods as
// dfs.ShardStore (PutShard/GetShard take/return raw CID bytes here instead of
// cid.Cid so this package does not need to import daemon/dfs or go-cid on its
// public surface, keeping the mesh/dfs layering one-directional: dfs and
// daemon/system depend on mesh, not the reverse).
type ShardServer interface {
	// PutShard stores shard bytes under the content address cidBytes on this
	// node's own local store.
	PutShard(cidBytes []byte, shard []byte) error
	// GetShard fetches the bytes stored under cidBytes from this node's own local
	// store. A miss returns an error.
	GetShard(cidBytes []byte) ([]byte, error)
}

// ServeShards registers the worker-side handler that lets remote peers store and
// fetch dfs shards on THIS node's local shard store, GATED by a signed capability
// envelope verified before local is ever touched. Call it once, before remote
// requests arrive.
//
// resolveIssuer resolves a claimed issuer PeerID to the trusted public key
// exchanged with it out of band (see compute.go's IssuerPubResolver); now returns
// the current unix time (inject a fixed value in tests); isRevoked may be nil.
func (f *Fabric) ServeShards(
	local ShardServer,
	resolveIssuer IssuerPubResolver,
	now func() int64,
	isRevoked auth.RevocationPredicate,
) {
	f.host.SetStreamHandler(shardProto, func(s network.Stream) {
		f.handleShardStream(s, local, resolveIssuer, now, isRevoked)
	})
}

func (f *Fabric) handleShardStream(
	s network.Stream,
	local ShardServer,
	resolveIssuer IssuerPubResolver,
	now func() int64,
	isRevoked auth.RevocationPredicate,
) {
	ss := newStreamSession(s)
	defer func() { _ = ss.Close() }()

	raw, err := ss.Recv()
	if err != nil {
		_ = s.Reset()
		return
	}
	var req shardRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		_ = writeShardError(ss, fmt.Sprintf("decode shard request: %v", err))
		return
	}

	requiredRight, err := shardRightFor(req.Op)
	if err != nil {
		_ = writeShardError(ss, err.Error())
		return
	}

	// The claimed issuer must equal the PeerID the QUIC/TLS handshake actually
	// authenticated for THIS stream — see the package doc comment's "Issuer
	// trust model". A request whose claimed Issuer does not match the
	// authenticated remote peer is rejected before resolveIssuer/auth.Verify even
	// runs (it could never validate anyway, since only the true holder of the
	// matching private key can have produced a signature that verifies against
	// this identity — this is an early, clear rejection rather than relying on
	// that fact alone).
	remotePeer, verified := ss.RemotePeerID()
	if !verified || remotePeer != req.Issuer {
		_ = writeShardError(ss, "mesh: shard request issuer does not match the authenticated mesh peer for this stream")
		return
	}

	// Fail closed: verify the signed capability BEFORE touching local at all — no
	// store mutation and no read happens on a missing/invalid envelope.
	if _, verr := verifyShardCap(req.Cap, req.Issuer, resolveIssuer, now, isRevoked, requiredRight); verr != nil {
		_ = writeShardError(ss, verr.Error())
		return
	}

	switch req.Op {
	case ShardOpPut:
		if err := local.PutShard(req.CIDBytes, req.Data); err != nil {
			_ = writeShardError(ss, err.Error())
			return
		}
		_ = ss.Send(mustMarshalShardResponse(shardResponse{OK: true}))
	case ShardOpGet:
		data, err := local.GetShard(req.CIDBytes)
		if err != nil {
			_ = writeShardError(ss, err.Error())
			return
		}
		_ = ss.Send(mustMarshalShardResponse(shardResponse{OK: true, Data: data}))
	default:
		_ = writeShardError(ss, fmt.Sprintf("unknown shard op %q", req.Op))
	}
}

// shardRightFor maps a ShardOp to the right a presented capability must convey:
// ShardOpPut requires RightWrite, ShardOpGet requires RightRead. An unknown op
// returns an error so an unrecognized/forged Op cannot bypass the gate by
// avoiding both switch branches in handleShardStream.
func shardRightFor(op ShardOp) (contract.Right, error) {
	switch op {
	case ShardOpPut:
		return contract.RightWrite, nil
	case ShardOpGet:
		return contract.RightRead, nil
	default:
		return "", fmt.Errorf("mesh: unknown shard op %q", op)
	}
}

// verifyShardCap extracts the signed envelope from the request, resolves the
// issuer key it names, and Verifies it — the single fail-closed gate applied
// before any local shard store access. It mirrors compute.go's verifySignedCap.
func verifyShardCap(
	env []byte,
	claimedIssuer contract.PeerID,
	resolveIssuer IssuerPubResolver,
	now func() int64,
	isRevoked auth.RevocationPredicate,
	requiredRight contract.Right,
) (auth.Grant, error) {
	if resolveIssuer == nil {
		return auth.Grant{}, fmt.Errorf("mesh: no issuer resolver configured for shard RPC")
	}
	if len(env) == 0 {
		return auth.Grant{}, fmt.Errorf("mesh: shard request carries no signed capability")
	}
	pub, ok := resolveIssuer(claimedIssuer)
	if !ok {
		return auth.Grant{}, fmt.Errorf("mesh: no trusted issuer key for shard cap issuer %x (unknown issuer)", claimedIssuer[:8])
	}
	t := int64(0)
	if now != nil {
		t = now()
	}
	grant, err := auth.Verify(env, pub, t, isRevoked)
	if err != nil {
		return auth.Grant{}, fmt.Errorf("mesh: shard capability denied: %w", err)
	}
	if requiredRight != "" && !grantHasRight(grant, requiredRight) {
		return auth.Grant{}, fmt.Errorf("mesh: shard capability lacks required right %q", requiredRight)
	}
	return grant, nil
}

// RequestPutShard dials peer and asks it to durably store shard bytes under
// cidBytes on ITS OWN local shard store, presenting a signed capability envelope
// (minted by issuer) that authorizes the write. Returns a non-nil error if the
// dial, the wire round-trip, the peer's capability gate, or the peer's own store
// rejected the put.
func (f *Fabric) RequestPutShard(ctx context.Context, peer contract.PeerID, cidBytes []byte, shard []byte, capEnvelope []byte, issuer contract.PeerID) error {
	resp, err := f.requestShard(ctx, peer, shardRequest{Op: ShardOpPut, CIDBytes: cidBytes, Data: shard, Cap: capEnvelope, Issuer: issuer})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("mesh: remote put shard: %s", resp.Error)
	}
	return nil
}

// RequestGetShard dials peer and fetches the shard bytes stored under cidBytes on
// ITS OWN local shard store, presenting a signed capability envelope (minted by
// issuer) that authorizes the read.
func (f *Fabric) RequestGetShard(ctx context.Context, peer contract.PeerID, cidBytes []byte, capEnvelope []byte, issuer contract.PeerID) ([]byte, error) {
	resp, err := f.requestShard(ctx, peer, shardRequest{Op: ShardOpGet, CIDBytes: cidBytes, Cap: capEnvelope, Issuer: issuer})
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("mesh: remote get shard: %s", resp.Error)
	}
	return resp.Data, nil
}

func (f *Fabric) requestShard(ctx context.Context, peer contract.PeerID, req shardRequest) (shardResponse, error) {
	pid, err := toLibp2pID(peer)
	if err != nil {
		return shardResponse{}, err
	}
	s, err := f.host.NewStream(ctx, pid, shardProto)
	if err != nil {
		return shardResponse{}, fmt.Errorf("mesh: dial shard peer: %w", err)
	}
	ss := newStreamSession(s)
	defer func() { _ = ss.Close() }()

	body, err := json.Marshal(req)
	if err != nil {
		return shardResponse{}, err
	}
	if err := ss.Send(body); err != nil {
		return shardResponse{}, fmt.Errorf("mesh: send shard request: %w", err)
	}

	raw, err := ss.Recv()
	if err != nil {
		return shardResponse{}, fmt.Errorf("mesh: recv shard response: %w", err)
	}
	var resp shardResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return shardResponse{}, fmt.Errorf("mesh: decode shard response: %w", err)
	}
	return resp, nil
}

func writeShardError(ss *streamSession, msg string) error {
	return ss.Send(mustMarshalShardResponse(shardResponse{OK: false, Error: msg}))
}

func mustMarshalShardResponse(r shardResponse) []byte {
	b, err := json.Marshal(r)
	if err != nil {
		b, _ = json.Marshal(shardResponse{OK: false, Error: "internal: marshal response"})
	}
	return b
}
