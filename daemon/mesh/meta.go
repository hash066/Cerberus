package mesh

// meta.go adds a capability-gated request/response for /cer/fs metadata (path→
// Manifest) replication over the same libp2p/QUIC stream mechanism shard.go and
// compute.go use. When node B writes a file, its Manifest is replicated to mesh
// peers so node A can resolve the path and run dfs.Get (fetching shards that may
// live on B) without a separate metadata store cluster.
//
// What is REAL here:
//   - PutMeta/GetMeta/ListMeta over encrypted libp2p/QUIC streams, gated by a
//     signed capability envelope verified BEFORE any local metadata store access.
//   - The serving side writes/reads the node's own local MetaStore (Bolt or mem).
//
// What is a STUB / not yet here (not faked):
//   - CRDT/transactional metadata with concurrent-write locks (vertical 04 §3).
//     v0.1 replicates whole Manifest records; last-writer-wins on the same path.
//   - Gossip-style passive replication; v0.1 uses explicit push-on-write and
//     pull-on-miss from known peers only.

import (
	"context"
	"encoding/json"
	"fmt"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/libp2p/go-libp2p/core/network"
)

const metaProto = "/cerberus/fs-meta/1.0.0"

// MeshMetaResource returns the per-site resource a metadata RPC capability must
// name: contract.KindFS at "cerberus/<site>/mesh-meta".
func MeshMetaResource(site string) contract.ResourceRef {
	return contract.ResourceRef{Kind: contract.KindFS, Path: "cerberus/" + site + "/mesh-meta"}
}

// MetaOp identifies which metadata operation a metaRequest performs.
type MetaOp string

const (
	MetaOpPut  MetaOp = "put"
	MetaOpGet  MetaOp = "get"
	MetaOpList MetaOp = "list"
)

type metaRequest struct {
	Op       MetaOp          `json:"op"`
	Path     string          `json:"path,omitempty"`
	Manifest json.RawMessage `json:"manifest,omitempty"`
	Cap      []byte          `json:"cap,omitempty"`
	Issuer   contract.PeerID `json:"issuer,omitempty"`
}

type metaResponse struct {
	OK       bool            `json:"ok"`
	Manifest json.RawMessage `json:"manifest,omitempty"`
	Paths    []string        `json:"paths,omitempty"`
	Error    string          `json:"error,omitempty"`
}

// MetaServer is the local /cer/fs metadata store a node exposes to remote peers.
type MetaServer interface {
	PutMeta(path string, manifestJSON []byte) error
	GetMeta(path string) ([]byte, bool)
	ListMeta() ([]string, error)
}

// ServeMeta registers the worker-side handler for remote metadata put/get/list,
// gated by a signed capability envelope verified before local is touched.
func (f *Fabric) ServeMeta(
	local MetaServer,
	resolveIssuer IssuerPubResolver,
	now func() int64,
	isRevoked auth.RevocationPredicate,
) {
	f.host.SetStreamHandler(metaProto, func(s network.Stream) {
		f.handleMetaStream(s, local, resolveIssuer, now, isRevoked)
	})
}

func (f *Fabric) handleMetaStream(
	s network.Stream,
	local MetaServer,
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
	var req metaRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		_ = writeMetaError(ss, fmt.Sprintf("decode meta request: %v", err))
		return
	}

	requiredRight, err := metaRightFor(req.Op)
	if err != nil {
		_ = writeMetaError(ss, err.Error())
		return
	}

	remotePeer, verified := ss.RemotePeerID()
	if !verified || remotePeer != req.Issuer {
		_ = writeMetaError(ss, "mesh: meta request issuer does not match the authenticated mesh peer for this stream")
		return
	}

	// Fail closed BEFORE any local metadata access, and only for a cap actually
	// issued for THIS site's metadata service — not merely one that happens to
	// carry the right right for something else.
	if _, verr := verifyMetaCap(req.Cap, req.Issuer, resolveIssuer, now, isRevoked, requiredRight, MeshMetaResource(f.site)); verr != nil {
		_ = writeMetaError(ss, verr.Error())
		return
	}

	switch req.Op {
	case MetaOpPut:
		if req.Path == "" || len(req.Manifest) == 0 {
			_ = writeMetaError(ss, "mesh: put meta requires path and manifest")
			return
		}
		if err := local.PutMeta(req.Path, req.Manifest); err != nil {
			_ = writeMetaError(ss, err.Error())
			return
		}
		_ = ss.Send(mustMarshalMetaResponse(metaResponse{OK: true}))
	case MetaOpGet:
		if req.Path == "" {
			_ = writeMetaError(ss, "mesh: get meta requires path")
			return
		}
		man, ok := local.GetMeta(req.Path)
		if !ok {
			_ = writeMetaError(ss, "mesh: meta not found: "+req.Path)
			return
		}
		_ = ss.Send(mustMarshalMetaResponse(metaResponse{OK: true, Manifest: man}))
	case MetaOpList:
		paths, err := local.ListMeta()
		if err != nil {
			_ = writeMetaError(ss, err.Error())
			return
		}
		_ = ss.Send(mustMarshalMetaResponse(metaResponse{OK: true, Paths: paths}))
	default:
		_ = writeMetaError(ss, fmt.Sprintf("unknown meta op %q", req.Op))
	}
}

func metaRightFor(op MetaOp) (contract.Right, error) {
	switch op {
	case MetaOpPut:
		return contract.RightWrite, nil
	case MetaOpGet, MetaOpList:
		return contract.RightRead, nil
	default:
		return "", fmt.Errorf("mesh: unknown meta op %q", op)
	}
}

// verifyMetaCap resolves the issuer key, Verifies the envelope, enforces the
// required right, and checks the grant is scoped to wantResource
// (MeshMetaResource(site)) — without that last step a cap minted for any other
// KindFS resource carrying RightWrite could rewrite this node's /cer/fs
// manifests. See capscope.go.
func verifyMetaCap(
	env []byte,
	claimedIssuer contract.PeerID,
	resolveIssuer IssuerPubResolver,
	now func() int64,
	isRevoked auth.RevocationPredicate,
	requiredRight contract.Right,
	wantResource contract.ResourceRef,
) (auth.Grant, error) {
	if resolveIssuer == nil {
		return auth.Grant{}, fmt.Errorf("mesh: no issuer resolver configured for meta RPC")
	}
	if len(env) == 0 {
		return auth.Grant{}, fmt.Errorf("mesh: meta request carries no signed capability")
	}
	pub, ok := resolveIssuer(claimedIssuer)
	if !ok {
		return auth.Grant{}, fmt.Errorf("mesh: no trusted issuer key for meta cap issuer %x (unknown issuer)", claimedIssuer[:8])
	}
	t := int64(0)
	if now != nil {
		t = now()
	}
	grant, err := auth.Verify(env, pub, t, isRevoked)
	if err != nil {
		return auth.Grant{}, fmt.Errorf("mesh: meta capability denied: %w", err)
	}
	if requiredRight != "" && !grantHasRight(grant, requiredRight) {
		return auth.Grant{}, fmt.Errorf("mesh: meta capability lacks required right %q", requiredRight)
	}
	if err := grantCoversResource("meta", grant, wantResource); err != nil {
		return auth.Grant{}, err
	}
	return grant, nil
}

// RequestPutMeta asks peer to durably record manifestJSON for path.
func (f *Fabric) RequestPutMeta(ctx context.Context, peer contract.PeerID, path string, manifestJSON []byte, capEnvelope []byte, issuer contract.PeerID) error {
	resp, err := f.requestMeta(ctx, peer, metaRequest{Op: MetaOpPut, Path: path, Manifest: manifestJSON, Cap: capEnvelope, Issuer: issuer})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("mesh: remote put meta: %s", resp.Error)
	}
	return nil
}

// RequestGetMeta fetches the manifest JSON for path from peer's local store.
func (f *Fabric) RequestGetMeta(ctx context.Context, peer contract.PeerID, path string, capEnvelope []byte, issuer contract.PeerID) ([]byte, error) {
	resp, err := f.requestMeta(ctx, peer, metaRequest{Op: MetaOpGet, Path: path, Cap: capEnvelope, Issuer: issuer})
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("mesh: remote get meta: %s", resp.Error)
	}
	return resp.Manifest, nil
}

// RequestListMeta returns every /cer/fs path peer knows about.
func (f *Fabric) RequestListMeta(ctx context.Context, peer contract.PeerID, capEnvelope []byte, issuer contract.PeerID) ([]string, error) {
	resp, err := f.requestMeta(ctx, peer, metaRequest{Op: MetaOpList, Cap: capEnvelope, Issuer: issuer})
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("mesh: remote list meta: %s", resp.Error)
	}
	return resp.Paths, nil
}

func (f *Fabric) requestMeta(ctx context.Context, peer contract.PeerID, req metaRequest) (metaResponse, error) {
	pid, err := toLibp2pID(peer)
	if err != nil {
		return metaResponse{}, err
	}
	s, err := f.host.NewStream(ctx, pid, metaProto)
	if err != nil {
		return metaResponse{}, fmt.Errorf("mesh: dial meta peer: %w", err)
	}
	ss := newStreamSession(s)
	defer func() { _ = ss.Close() }()

	body, err := json.Marshal(req)
	if err != nil {
		return metaResponse{}, err
	}
	if err := ss.Send(body); err != nil {
		return metaResponse{}, fmt.Errorf("mesh: send meta request: %w", err)
	}

	raw, err := ss.Recv()
	if err != nil {
		return metaResponse{}, fmt.Errorf("mesh: recv meta response: %w", err)
	}
	var resp metaResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return metaResponse{}, fmt.Errorf("mesh: decode meta response: %w", err)
	}
	return resp, nil
}

func writeMetaError(ss *streamSession, msg string) error {
	return ss.Send(mustMarshalMetaResponse(metaResponse{OK: false, Error: msg}))
}

func mustMarshalMetaResponse(r metaResponse) []byte {
	b, err := json.Marshal(r)
	if err != nil {
		b, _ = json.Marshal(metaResponse{OK: false, Error: "internal: marshal response"})
	}
	return b
}
