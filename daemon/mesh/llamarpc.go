package mesh

// llamarpc.go carries llama.cpp's OWN ggml-rpc protocol between two mesh peers
// over a capability-gated libp2p/QUIC stream. It is the transport half of real
// distributed LLM inference: daemon/llama supervises the binaries, this file moves
// their bytes and decides who is allowed to.
//
// Shape-wise this is audio.go, not compute.go/shard.go. Those exchange a single
// request/response frame. An offload session, like an audio session, is a
// CONTINUOUS bidirectional byte stream: after the capability gate passes, the raw
// QUIC stream is spliced to a local TCP connection and Cerberus stops framing
// anything. ggml-rpc has its own framing and its own request/response discipline;
// imposing a second layer on top would corrupt it.
//
// WHY THE HAND-OFF IS SAFE: streamSession deliberately does NOT wrap the stream in
// a bufio.Reader — session.go's Recv calls io.ReadFull on the stream directly. So
// when the gate passes and we hand the raw stream to io.Copy, there are no bytes
// stranded in a buffer that the copy would miss. If anyone ever adds buffering to
// streamSession, this file and audio.go both break in a way that looks like data
// corruption rather than a compile error. Do not add it.
//
// WHAT IS REAL: the transport is the same authenticated libp2p host over QUIC that
// compute/shard/audio use — encrypted, multiplexed, and PeerID-authenticated by
// the QUIC/TLS handshake. The bytes are genuine ggml-rpc traffic between a real
// llama-server and a real ggml-rpc-server on another machine. Cerberus is not
// simulating an LLM anywhere on this path; it does not know what a token is.
//
// CAPABILITY GATE (identical discipline to shard.go / audio.go):
//   - Every session REQUIRES a signed capability envelope (daemon/auth.SignedCap,
//     Ed25519-signed), verified server-side BEFORE any process is spawned and
//     BEFORE any local socket is dialed. Nothing starts on an invalid envelope.
//   - Issuer trust model: an offload session is symmetric peer-to-peer traffic with
//     no grant-exchange side-channel, so the server binds the claimed issuer to the
//     PeerID the QUIC/TLS handshake ALREADY authenticated for THIS stream
//     (streamSession.RemotePeerID()) and verifies the envelope as self-signed by
//     that peer via SelfIssuerResolver.
//   - Resource/right: contract.KindGPU at "cerberus/<site>/llama-rpc", requiring
//     contract.RightExec. KindGPU reuses the frozen contract's existing kind rather
//     than minting a new one (that would be a cross-lane contract change), and it
//     is the honest kind: daemon/gpu already uses KindGPU for "this node's best
//     available compute backend, wgpu or cpu-software", which is exactly what a
//     ggml-rpc worker offers.
//
// RightExec IS MEANT LITERALLY. ggml-rpc's deserializer trusts its peer;
// CVE-2026-34159 was a 9.8-severity unauthenticated RCE in it. Handing a peer a
// capability for this resource is handing it the ability to run code on your
// machine if it is malicious or compromised. The gate reduces the attack surface
// from "any host that can reach the port" to "peers you issued a capability to".
// It does NOT make the C++ deserializer safe, and this is NOT a sandbox. See
// daemon/llama/doc.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/libp2p/go-libp2p/core/network"
)

// llamaRPCProto is the libp2p protocol id for a capability-gated llama.cpp offload
// session.
const llamaRPCProto = "/cerberus/llama/rpc/1.0.0"

// LlamaRPCResource returns the per-site resource an offload capability must name:
// contract.KindGPU at "cerberus/<site>/llama-rpc". Callers outside this package
// (the daemon RPC) mint a signed RightExec capability against exactly this
// Kind/Path, so ServeLlamaRPC's gate accepts it.
func LlamaRPCResource(site string) contract.ResourceRef {
	return contract.ResourceRef{Kind: contract.KindGPU, Path: "cerberus/" + site + "/llama-rpc"}
}

// llamaRPCRequest is the on-wire session-open frame, sent by the requester as the
// FIRST length-prefixed message. After the server ACKs, the stream carries raw
// ggml-rpc bytes and nothing else.
type llamaRPCRequest struct {
	// Build is the requester's llama.cpp build number. See the skew check in
	// handleLlamaRPCStream.
	Build int `json:"build"`
	// Cap is the signed capability envelope authorizing the session.
	Cap []byte `json:"cap,omitempty"`
	// Issuer names the PeerID that minted Cap; it must equal the authenticated
	// remote peer.
	Issuer contract.PeerID `json:"issuer,omitempty"`
}

// llamaRPCAck is the server's single reply frame before the byte splice. OK=false
// means the gate denied the session, the builds are skewed, or the local worker
// could not be started; the requester aborts without sending ggml-rpc bytes.
type llamaRPCAck struct {
	OK    bool   `json:"ok"`
	Build int    `json:"build,omitempty"`
	Error string `json:"error,omitempty"`
}

// LlamaBackend is the local llama.cpp worker boundary the serving node exposes to
// a remote peer. It is INJECTED so this package imports no llama code and stays a
// leaf — the same discipline that keeps AudioServer/GpuHandler out of mesh.
// daemon/llama implements it (see llama.WireWorker).
type LlamaBackend interface {
	// Build returns the llama.cpp build number this node's binaries report.
	Build() int
	// OpenLocal admits ONE offload session, lazily starting the local
	// ggml-rpc-server, and returns a connection to it. The caller closes the
	// returned value exactly once when the session ends, which must also release
	// the admission slot and terminate the worker.
	//
	// grant is the VERIFIED capability that authorized this session. It is passed
	// so the backend can apply the grant's own bounds (see daemon/llama/quota.go).
	// The gate has already run: an implementation must not re-decide whether the
	// peer is allowed, only what its grant permits.
	//
	// It returns an error (daemon/llama.ErrBusy) if a session is already active.
	// Implementations MUST NOT queue: a queued peer sits on an open socket
	// producing nothing while llama.cpp's client side runs its own timeouts.
	OpenLocal(ctx context.Context, grant auth.Grant) (io.ReadWriteCloser, error)
}

// ServeLlamaRPC registers the responder side of cross-node llama.cpp offload,
// GATED by a signed capability verified before any process is spawned or any
// socket is dialed. Call it once, before remote requests arrive.
//
// This is OFF by default in the daemon (-llama-worker). Enabling it grants code
// execution to every peer holding a valid capability for this node's llama-rpc
// resource. That is not a slogan; see the CVE note in the file header.
//
// resolveIssuer resolves a claimed issuer PeerID to the trusted public key
// (SelfIssuerResolver here, since the issuer must be the authenticated remote
// peer); now returns the current unix time (inject a fixed value in tests);
// isRevoked may be nil.
func (f *Fabric) ServeLlamaRPC(
	backend LlamaBackend,
	resolveIssuer IssuerPubResolver,
	now func() int64,
	isRevoked auth.RevocationPredicate,
) {
	f.host.SetStreamHandler(llamaRPCProto, func(s network.Stream) {
		f.handleLlamaRPCStream(s, backend, resolveIssuer, now, isRevoked)
	})
}

func (f *Fabric) handleLlamaRPCStream(
	s network.Stream,
	backend LlamaBackend,
	resolveIssuer IssuerPubResolver,
	now func() int64,
	isRevoked auth.RevocationPredicate,
) {
	ss := newStreamSession(s)
	// NOTE: do not defer ss.Close() before the splice phase — the copy owns the
	// stream bytes; closing happens after it returns.

	raw, err := ss.Recv()
	if err != nil {
		_ = s.Reset()
		return
	}
	var req llamaRPCRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		_ = writeLlamaRPCAck(ss, fmt.Sprintf("decode llama rpc request: %v", err))
		_ = ss.Close()
		return
	}

	// The claimed issuer must equal the PeerID the QUIC/TLS handshake actually
	// authenticated for THIS stream — same issuer trust model as shard.go/audio.go.
	// Rejected before resolveIssuer/auth.Verify even runs.
	remotePeer, verified := ss.RemotePeerID()
	if !verified || remotePeer != req.Issuer {
		_ = writeLlamaRPCAck(ss, "mesh: llama rpc request issuer does not match the authenticated mesh peer for this stream")
		_ = ss.Close()
		return
	}

	// FAIL CLOSED. This is the line that matters: no ggml-rpc-server process is
	// spawned and no loopback socket is dialed until this returns nil. Everything
	// downstream of here is reachable only by a peer holding a valid, unexpired,
	// unrevoked RightExec capability for this node's llama-rpc resource.
	grant, verr := verifyLlamaRPCCap(req.Cap, req.Issuer, resolveIssuer, now, isRevoked, contract.RightExec, LlamaRPCResource(f.site))
	if verr != nil {
		_ = writeLlamaRPCAck(ss, verr.Error())
		_ = ss.Close()
		return
	}

	if backend == nil {
		_ = writeLlamaRPCAck(ss, "mesh: llama worker is not enabled on this node (start cerberusd with -llama-worker)")
		_ = ss.Close()
		return
	}

	// VERSION SKEW: refuse loudly rather than letting a C++ deserializer meet bytes
	// from a build it does not agree with. ggml-rpc's wire format is an internal
	// protocol between matched binaries, not a stable inter-version contract; a
	// mismatch does not fail cleanly, it misparses. Cheap check, catastrophic
	// failure mode avoided.
	if local := backend.Build(); req.Build != local {
		_ = writeLlamaRPCAck(ss, fmt.Sprintf(
			"mesh: llama.cpp build mismatch — requester is b%d, this worker is b%d. "+
				"ggml-rpc's wire format is not stable across builds; both nodes must run the same "+
				"Cerberus llama pack.", req.Build, local))
		_ = ss.Close()
		return
	}

	// Admission control + lazy spawn live in the backend. A second concurrent
	// session is refused here, not queued.
	local, oerr := backend.OpenLocal(f.ctx, grant)
	if oerr != nil {
		_ = writeLlamaRPCAck(ss, "mesh: "+oerr.Error())
		_ = ss.Close()
		return
	}
	defer local.Close()

	// Authorized: ACK, then splice. The ACK precedes the bytes so the requester
	// knows the gate passed before llama-server starts talking ggml-rpc.
	if err := ss.Send(mustMarshalLlamaRPCAck(llamaRPCAck{OK: true, Build: backend.Build()})); err != nil {
		_ = s.Reset()
		return
	}

	if err := spliceLlamaRPC(s, local); err != nil {
		// The splice phase already owns the stream bytes; an ack frame here would
		// be interpreted as ggml-rpc payload and corrupt the stream. Reset is the
		// only honest signal — same reasoning as audio.go.
		_ = s.Reset()
		return
	}
	_ = ss.Close()
}

// spliceLlamaRPC pumps bytes both ways between the mesh stream and the local
// ggml-rpc connection until either side ends. NO framing is applied: from here on
// the stream is ggml-rpc's, byte for byte.
func spliceLlamaRPC(remote io.ReadWriter, local io.ReadWriter) error {
	errc := make(chan error, 2)
	go func() {
		_, err := io.Copy(local, remote) // peer -> local worker
		errc <- err
	}()
	go func() {
		_, err := io.Copy(remote, local) // local worker -> peer
		errc <- err
	}()
	// The first direction to finish ends the session. The deferred Close on the
	// local conn (and the stream Reset/Close in the caller) unblocks the other
	// copy, so we do not wait for it: a half-open ggml-rpc session is useless to
	// both ends.
	err := <-errc
	if err == io.EOF {
		return nil
	}
	return err
}

// verifyLlamaRPCCap extracts the signed envelope, resolves the issuer key it
// names, Verifies it, and checks that the grant is actually FOR this resource.
//
// HISTORY, because it explains why this file used to shout: this gate was for a
// while the ONLY one in the package that compared grant.Resource to what it was
// guarding. verifyAudioCap, verifyShardCap, verifyMetaCap, verifyComponentFetchCap
// and verifySignedCap all stopped after the right check, and audio.go/shard.go
// discarded the verified grant entirely. That is fixed: every gate in this package
// now scopes, through the single shared check in capscope.go, and this one no
// longer carries a bespoke copy of the rule (it previously compared Kind and Path
// but not Node — a third variant nobody needed).
//
// The stakes here remain the highest in the package, which is why the warning
// stays: a peer holding any valid signed cap carrying RightExec — say one issued
// for MeshComputeResource(site) so it could run a WASM workload — would otherwise
// pass this gate and receive a ggml-rpc-server session. Given CVE-2026-34159 that
// is a pre-auth RCE surface reachable with an unrelated capability, and it would
// make this lane's central claim ("only a peer holding a capability FOR llama
// offload") simply false. A capability that is not checked against its resource is
// not a capability; it is a signed permission slip.
func verifyLlamaRPCCap(
	env []byte,
	claimedIssuer contract.PeerID,
	resolveIssuer IssuerPubResolver,
	now func() int64,
	isRevoked auth.RevocationPredicate,
	requiredRight contract.Right,
	wantResource contract.ResourceRef,
) (auth.Grant, error) {
	if resolveIssuer == nil {
		return auth.Grant{}, fmt.Errorf("mesh: no issuer resolver configured for llama rpc session")
	}
	if len(env) == 0 {
		return auth.Grant{}, fmt.Errorf("mesh: llama rpc request carries no signed capability")
	}
	pub, ok := resolveIssuer(claimedIssuer)
	if !ok {
		return auth.Grant{}, fmt.Errorf("mesh: no trusted issuer key for llama rpc cap issuer %x (unknown issuer)", claimedIssuer[:8])
	}
	t := int64(0)
	if now != nil {
		t = now()
	}
	grant, err := auth.Verify(env, pub, t, isRevoked)
	if err != nil {
		return auth.Grant{}, fmt.Errorf("mesh: llama rpc capability denied: %w", err)
	}
	if requiredRight != "" && !grantHasRight(grant, requiredRight) {
		return auth.Grant{}, fmt.Errorf("mesh: llama rpc capability lacks required right %q", requiredRight)
	}
	if err := grantCoversResource("llama rpc", grant, wantResource); err != nil {
		return auth.Grant{}, err
	}
	return grant, nil
}

// OpenLlamaRPCSession dials peer, presents the signed capability, and on ACK
// splices local to the mesh stream. It blocks until the session ends.
//
// local is one accepted connection from llama-server (see daemon/llama's
// forwarder). Ownership: this function does NOT close local; the caller does.
func (f *Fabric) OpenLlamaRPCSession(
	peer contract.PeerID,
	capEnvelope []byte,
	issuer contract.PeerID,
	build int,
	local io.ReadWriter,
) error {
	pid, err := toLibp2pID(peer)
	if err != nil {
		return err
	}
	s, err := f.host.NewStream(f.ctx, pid, llamaRPCProto)
	if err != nil {
		return contract.Errf(contract.ErrPartitioned, err.Error())
	}
	ss := newStreamSession(s)

	body, err := json.Marshal(llamaRPCRequest{Build: build, Cap: capEnvelope, Issuer: issuer})
	if err != nil {
		_ = s.Reset()
		return err
	}
	if err := ss.Send(body); err != nil {
		_ = s.Reset()
		return contract.Errf(contract.ErrPartitioned, err.Error())
	}

	raw, err := ss.Recv()
	if err != nil {
		_ = s.Reset()
		return err
	}
	var ack llamaRPCAck
	if err := json.Unmarshal(raw, &ack); err != nil {
		_ = s.Reset()
		return fmt.Errorf("mesh: decode llama rpc ack: %w", err)
	}
	if !ack.OK {
		_ = ss.Close()
		return fmt.Errorf("mesh: llama rpc session denied by peer: %s", ack.Error)
	}

	// Gate passed — splice this llama-server connection to the remote worker.
	serr := spliceLlamaRPC(s, local)
	_ = ss.Close()
	return serr
}

func writeLlamaRPCAck(ss *streamSession, msg string) error {
	return ss.Send(mustMarshalLlamaRPCAck(llamaRPCAck{OK: false, Error: msg}))
}

func mustMarshalLlamaRPCAck(a llamaRPCAck) []byte {
	b, err := json.Marshal(a)
	if err != nil {
		b, _ = json.Marshal(llamaRPCAck{OK: false, Error: "internal: marshal llama rpc ack"})
	}
	return b
}
