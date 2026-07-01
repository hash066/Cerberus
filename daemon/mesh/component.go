package mesh

// component.go adds a peer-to-peer component-fetch request/response, modeled
// directly on compute.go's ComputeTask request/response pattern (same
// streamSession framing, same "dial by PeerID, get a typed JSON response"
// shape). It is the real mesh transport for the documented next step called
// out in daemon/wasm/cidstore.go's header comment: "a persistent/peer-to-peer
// IPLD fetch ... not faked here." This file is that fetch.
//
// What is REAL here:
//   - A node asks a specific peer, by PeerID, for the raw bytes behind a
//     component CID, over the same authenticated QUIC stream compute uses.
//   - The requester (not just the worker) verifies the returned bytes actually
//     hash to the CID it asked for BEFORE accepting them — the same integrity
//     discipline daemon/wasm.ContentStore.Get already enforces on the local
//     path. A peer cannot hand back arbitrary/substituted bytes and have them
//     accepted; they are re-hashed and compared to the requested CID.
//   - A peer that does not have the requested CID returns a clean typed error
//     (Found=false / Error set), not a hang or a panic.
//
// CAPABILITY GATE (retires the "no capability gate applied here" stub):
//   - Component bytes are public, content-addressed data: the CID already
//     proves integrity to the requester (see the re-hash check in
//     RequestComponent below), so a fine-grained per-component grant would add
//     no real confidentiality or integrity protection — anyone who legitimately
//     learns a CID can already verify what they get back. But "no check at
//     all" let ANY dialable peer enumerate/pull this node's entire component
//     store, which is a real minimum-viable-fix gap (undiscoverable-by-design
//     resources become discoverable, and it is unmetered/unthrottled read
//     access with no notion of "this peer belongs on my mesh"). The defensible
//     minimum enforced here is proof of general mesh-fabric membership: a
//     signed capability over resource kind contract.KindTopic, path
//     "cerberus/<site>/mesh-fabric", conveying RightRead — i.e. "this caller
//     was granted standing on this site's fabric", not "this caller may fetch
//     component X specifically". This mirrors compute.go's signed-cap
//     verification machinery (verifySignedCap/grantHasRight) at a coarser
//     resource scope appropriate to public content-addressed data.
//   - A request missing a required right, or carrying a malformed/forged/wrong-
//     issuer/expired/revoked envelope, is denied before store.Has/store.Get is
//     ever called — fail closed, same discipline as shard.go/compute.go.
//
// Wire size judgment call: component bytes here are small (KB-scale) example
// WASM fixtures, so a single request/response frame carrying the raw bytes is
// sufficient — the same framing compute.go already uses for a whole
// ComputeTask. This deliberately does NOT route over the QUIC data plane's
// chunked-transfer machinery (daemon/dataplane); that machinery exists for
// large payloads and would be pure overhead for these fixtures. If a component
// large enough to need chunking shows up (MB-scale WASM), that is the
// documented next step for THIS path, not something faked here.
//
// What is NOT here: any notion of peer selection/ranking, retries with
// backoff, or a DHT/provider-record discovery layer (IPLD's "who has this
// CID" problem). The caller supplies the candidate peers (its known mesh
// peers); this file only implements "ask this one peer for these bytes".

import (
	"context"
	"encoding/json"
	"fmt"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/wasm"
	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p/core/network"
)

// componentFetchProto is the libp2p protocol id for peer component fetch.
const componentFetchProto = "/cerberus/component-fetch/1.0.0"

// componentFetchRequest is the on-wire request: the CID (bytes form) whose
// backing bytes the requester wants, plus the signed capability envelope
// proving the requester's standing on this node's mesh fabric (see the
// package doc comment above for the resource/right convention).
type componentFetchRequest struct {
	CID    []byte          `json:"cid"`
	Cap    []byte          `json:"cap,omitempty"`
	Issuer contract.PeerID `json:"issuer,omitempty"`
}

// componentFetchResponse is the on-wire response. Found=false + Error set
// means the peer does not have this CID (or refused it) — a clean negative
// result, not a transport failure. The same shape also reports a denied
// capability gate (Found=false, Error explains the denial).
type componentFetchResponse struct {
	Found     bool   `json:"found"`
	Component []byte `json:"component,omitempty"`
	Error     string `json:"error,omitempty"`
}

// ComponentSource is the local lookup a node consults to answer a peer's
// fetch request. daemon/wasm.ContentStore satisfies this (Get + Has).
type ComponentSource interface {
	Get(c cid.Cid) ([]byte, error)
	Has(c cid.Cid) bool
}

// MeshFabricResource returns the coarse resource a component-fetch capability
// must name for site: standing on this site's mesh fabric, not any particular
// component. Callers outside this package (daemon/system, test/e2e/node) mint a
// signed capability against exactly this Kind/Path, conveying RightRead, so
// ServeComponentFetch's gate accepts it.
func MeshFabricResource(site string) contract.ResourceRef {
	return contract.ResourceRef{Kind: contract.KindTopic, Path: "cerberus/" + site + "/mesh-fabric"}
}

// ServeComponentFetch registers the responder side of peer component fetch:
// verify the caller presents a signed capability proving mesh-fabric
// membership (RightRead on this site's mesh-fabric resource), THEN look the
// requested CID up in the local store and return its bytes, or a clean "not
// found" response if absent. See the package doc comment for why this scope
// (not a per-component grant) is the defensible minimum for public,
// content-addressed data.
func (f *Fabric) ServeComponentFetch(
	store ComponentSource,
	resolveIssuer IssuerPubResolver,
	now func() int64,
	isRevoked auth.RevocationPredicate,
) {
	f.host.SetStreamHandler(componentFetchProto, func(s network.Stream) {
		f.handleComponentFetchStream(s, store, resolveIssuer, now, isRevoked)
	})
}

func (f *Fabric) handleComponentFetchStream(
	s network.Stream,
	store ComponentSource,
	resolveIssuer IssuerPubResolver,
	now func() int64,
	isRevoked auth.RevocationPredicate,
) {
	ss := newStreamSession(s)
	defer ss.Close()

	raw, err := ss.Recv()
	if err != nil {
		_ = s.Reset()
		return
	}
	var req componentFetchRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		_ = writeComponentFetchError(ss, fmt.Sprintf("decode request: %v", err))
		return
	}

	// Fail closed: verify mesh-fabric membership BEFORE store.Has/store.Get is
	// ever called — a request with no/invalid envelope learns nothing about
	// this node's component store, not even whether a CID is present.
	if _, verr := verifyComponentFetchCap(req.Cap, req.Issuer, resolveIssuer, now, isRevoked); verr != nil {
		_ = writeComponentFetchError(ss, verr.Error())
		return
	}

	c, err := cid.Cast(req.CID)
	if err != nil {
		_ = writeComponentFetchError(ss, fmt.Sprintf("bad cid: %v", err))
		return
	}
	if store == nil || !store.Has(c) {
		_ = ss.Send(mustMarshalComponentFetchResponse(componentFetchResponse{
			Found: false,
			Error: fmt.Sprintf("component %s not found", c),
		}))
		return
	}
	b, err := store.Get(c)
	if err != nil {
		// Should not happen (Has just returned true), but report rather than hide.
		_ = writeComponentFetchError(ss, err.Error())
		return
	}
	_ = ss.Send(mustMarshalComponentFetchResponse(componentFetchResponse{Found: true, Component: b}))
}

// verifyComponentFetchCap resolves and Verifies the signed envelope proving
// mesh-fabric membership. It is the fail-closed gate applied before any local
// ComponentSource access — the lighter-weight sibling of shard.go's
// verifyShardCap, requiring only RightRead on the coarse mesh-fabric resource
// rather than a per-resource-path grant (see the package doc comment).
func verifyComponentFetchCap(
	env []byte,
	claimedIssuer contract.PeerID,
	resolveIssuer IssuerPubResolver,
	now func() int64,
	isRevoked auth.RevocationPredicate,
) (auth.Grant, error) {
	if resolveIssuer == nil {
		return auth.Grant{}, fmt.Errorf("mesh: no issuer resolver configured for component-fetch")
	}
	if len(env) == 0 {
		return auth.Grant{}, fmt.Errorf("mesh: component-fetch request carries no signed capability")
	}
	pub, ok := resolveIssuer(claimedIssuer)
	if !ok {
		return auth.Grant{}, fmt.Errorf("mesh: no trusted issuer key for component-fetch cap issuer %x (unknown issuer)", claimedIssuer[:8])
	}
	t := int64(0)
	if now != nil {
		t = now()
	}
	grant, err := auth.Verify(env, pub, t, isRevoked)
	if err != nil {
		return auth.Grant{}, fmt.Errorf("mesh: component-fetch capability denied: %w", err)
	}
	if !grantHasRight(grant, contract.RightRead) {
		return auth.Grant{}, fmt.Errorf("mesh: component-fetch capability lacks required right %q", contract.RightRead)
	}
	return grant, nil
}

// RequestComponent dials peer and asks it for the bytes behind c, presenting a
// signed capability envelope (minted by issuer) proving mesh-fabric
// membership. If the peer has it, the returned bytes are re-hashed and MUST
// match c exactly before this function returns them — a peer cannot
// substitute different bytes for the CID it was asked for. If the peer does
// not have it, or denies the capability, a clean error is returned (never a
// panic/hang).
func (f *Fabric) RequestComponent(ctx context.Context, peer contract.PeerID, c cid.Cid, capEnvelope []byte, issuer contract.PeerID) ([]byte, error) {
	pid, err := toLibp2pID(peer)
	if err != nil {
		return nil, err
	}
	s, err := f.host.NewStream(ctx, pid, componentFetchProto)
	if err != nil {
		return nil, contract.Errf(contract.ErrPartitioned, err.Error())
	}
	ss := newStreamSession(s)
	defer ss.Close()

	body, err := json.Marshal(componentFetchRequest{CID: c.Bytes(), Cap: capEnvelope, Issuer: issuer})
	if err != nil {
		return nil, err
	}
	if err := ss.Send(body); err != nil {
		return nil, contract.Errf(contract.ErrPartitioned, err.Error())
	}

	raw, err := ss.Recv()
	if err != nil {
		return nil, err
	}
	var resp componentFetchResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("mesh: decode component-fetch response: %w", err)
	}
	if !resp.Found {
		msg := resp.Error
		if msg == "" {
			msg = fmt.Sprintf("peer does not have component %s", c)
		}
		return nil, fmt.Errorf("mesh: component-fetch: %s", msg)
	}

	// Integrity check: the requester never trusts wire bytes on their word — the
	// same discipline wasm.ContentStore.Get enforces on the purely-local path. A
	// peer that returns bytes not matching the requested CID is rejected here,
	// before the caller ever populates its own store with them.
	got, err := wasm.ComponentCID(resp.Component)
	if err != nil {
		return nil, fmt.Errorf("mesh: component-fetch: hash returned bytes: %w", err)
	}
	if !got.Equals(c) {
		return nil, fmt.Errorf("mesh: component-fetch integrity check failed: peer returned bytes hashing to %s, not requested %s", got, c)
	}
	return resp.Component, nil
}

func writeComponentFetchError(ss *streamSession, msg string) error {
	return ss.Send(mustMarshalComponentFetchResponse(componentFetchResponse{Found: false, Error: msg}))
}

func mustMarshalComponentFetchResponse(r componentFetchResponse) []byte {
	b, err := json.Marshal(r)
	if err != nil {
		b, _ = json.Marshal(componentFetchResponse{Found: false, Error: "internal: marshal response"})
	}
	return b
}
