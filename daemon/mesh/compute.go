package mesh

// compute.go adds a minimal, capability-gated request/response for compute
// dispatch over a point-to-point libp2p/QUIC stream. It is the mesh-transport
// realization of Vertical 03's control-plane task dispatch: a requester sends a
// contract.ComputeTask to a worker, the worker authorizes the presented
// capability and runs the task, and the result bytes come back over the same
// stream.
//
// What is REAL here:
//   - The transport is the existing libp2p host over QUIC (quic-v1): encrypted,
//     multiplexed, PeerID-authenticated by the QUIC/TLS handshake. We reuse the
//     same streamSession framing (4-byte length prefix) as the generic session.
//   - The capability is verified by the worker's CapKernel BEFORE any work runs
//     (no ambient authority: an unauthorized task is rejected with the kernel's
//     error). The cap handle travels in-band only because, in v0.1, both nodes
//     share one logical kernel namespace in the demo; the seam is the same one a
//     CapTP frame would use.
//
// CROSS-KERNEL SIGNED CAPABILITY TRANSFER (retires the shared-kernel demo stub):
//   - The requester now also attaches a SIGNED capability envelope (a
//     daemon/auth.SignedCap, Ed25519-signed by the granting node's issuer key) in
//     ComputeTask.Caps[capSlot]. The worker VERIFIES that envelope against the
//     granting node's issuer PUBLIC KEY — exchanged out-of-band at discovery — via
//     auth.Verify BEFORE running anything. This is the zero-trust invariant the
//     HANDOFF stub was missing: the worker cryptographically trusts a capability
//     even though the authority is not an opaque handle into a shared in-process
//     kernel. The signed envelope is now the authority on the wire; the opaque
//     kernel check is retained (belt-and-suspenders) only when a kernel gate is
//     also configured on the handler side.
//
// What is a STUB / not yet here (not faked):
//   - Promise pipelining (CapTP) across multiple shards is engine-side in Rust
//     (core/runtime, Phase E2); this path carries a single task to completion.
//   - The signing preimage is auth's deterministic length-prefixed encoding, not
//     yet the frozen canonical CBOR of schemas/capability.cddl (a contract-level
//     change, out of this lane — see daemon/auth/signedcap.go MATURITY HONESTY).

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/libp2p/go-libp2p/core/network"
)

// capSlot is the ComputeTask.Caps index carrying the primary authorizing signed
// capability envelope. A delegation chain would occupy the following slots.
const capSlot = 0

// computeProto is the libp2p protocol id for capability-gated compute dispatch.
const computeProto = "/cerberus/compute/1.0.0"

// MeshComputeResource returns the per-site resource a mesh compute dispatch
// capability must name: contract.KindGPU at "cerberus/<site>/mesh-compute".
// Production daemons mint a signed RightExec capability against this resource
// (self-issued under the mesh identity key, stream-bound — see shard.go's
// issuer trust model) before calling RequestComputeSigned; e2e nodes mint
// against a per-node wasm-exec path instead, but the wire shape is the same.
func MeshComputeResource(site string) contract.ResourceRef {
	return contract.ResourceRef{Kind: contract.KindGPU, Path: "cerberus/" + site + "/mesh-compute"}
}

// ComputeHandler runs a dispatched task on the worker side. It is given the task
// and the in-band capability handle the requester presented; the implementation
// is responsible for authorizing that capability (it owns the CapKernel that
// minted it) and for resolving the task's component CID to bytes before running.
//
// Returning a contract.ComputeResult with OK=false (or an error) both report
// failure; an error additionally aborts the stream. The result is sent back to
// the requester verbatim.
type ComputeHandler func(ctx context.Context, task contract.ComputeTask, capH contract.CapHandle) (contract.ComputeResult, error)

// IssuerPubResolver returns the Ed25519 public key of the node that issued a
// signed capability, given the issuer PeerID named inside the grant. It is how a
// worker obtains the "granting node's issuer public key" it must Verify against —
// the key exchanged out-of-band at discovery/handshake. Returning ok=false means
// the worker holds no trusted key for that issuer, so the cap is rejected before
// any work runs (a node will not trust a cap from an issuer it never met).
type IssuerPubResolver func(issuer contract.PeerID) (ed25519.PublicKey, bool)

// SignedComputeHandler is the worker-side callback invoked ONLY AFTER the signed
// capability envelope carried in the task has been cryptographically verified. It
// receives the verified auth.Grant (the trusted authority recovered from the
// envelope) in addition to the task, so the worker can enforce the grant's rights
// (e.g. that it actually conveys RightExec) without re-parsing bytes.
type SignedComputeHandler func(ctx context.Context, task contract.ComputeTask, grant auth.Grant) (contract.ComputeResult, error)

// computeRequest is the on-wire request frame: a ComputeTask plus the capability
// handle that authorizes its execution. JSON keeps it inspectable and
// dependency-free (the frozen CBOR/proto wire form is regenerated by lane D).
//
// Issuer names the node that minted the signed capability in Task.Caps[capSlot];
// the worker resolves that PeerID to the issuer public key it exchanged at
// discovery and Verifies the envelope under it. It is only a hint for key lookup
// — a false Issuer resolves either to no key (rejected) or to the wrong key
// (auth.Verify then fails), so it cannot forge authority.
type computeRequest struct {
	Task   computeTaskWire    `json:"task"`
	Cap    contract.CapHandle `json:"cap"`
	Issuer contract.PeerID    `json:"issuer,omitempty"`
}

// computeTaskWire is the JSON projection of contract.ComputeTask. We project
// explicitly rather than tagging the frozen contract struct so the contract is
// not edited from this lane.
type shardWire struct {
	Kind    uint8  `json:"kind"`
	LayerLo uint32 `json:"layer_lo,omitempty"`
	LayerHi uint32 `json:"layer_hi,omitempty"`
	TPRank  uint32 `json:"tp_rank,omitempty"`
	TPWorld uint32 `json:"tp_world,omitempty"`
}

type activationWire struct {
	Payload     []byte   `json:"payload,omitempty"`
	Shape       []uint32 `json:"shape,omitempty"`
	DType       uint8    `json:"dtype,omitempty"`
	Compression uint8    `json:"compression,omitempty"`
	StageIndex  uint32   `json:"stage_index,omitempty"`
}

type computeTaskWire struct {
	TaskID        []byte         `json:"task_id"`
	Component     []byte         `json:"component"` // IPLD CID bytes of the WASM component
	Shard         shardWire      `json:"shard,omitempty"`
	Caps          [][]byte       `json:"caps,omitempty"`
	Deps          []byte         `json:"deps,omitempty"` // v0.1: activation transfer id (8 bytes LE) when set
	Input         []byte         `json:"input,omitempty"`
	ResultCap     []byte         `json:"result_cap,omitempty"`
	Activation    activationWire `json:"activation,omitempty"`
	PipelineStage uint32         `json:"pipeline_stage,omitempty"`
}

func activationToWire(f contract.ActivationFrame) activationWire {
	return activationWire{
		Payload: append([]byte(nil), f.Payload...),
		Shape:   append([]uint32(nil), f.Shape...),
		DType:   uint8(f.DType), Compression: uint8(f.Compression), StageIndex: f.StageIndex,
	}
}

func (w activationWire) toActivation() contract.ActivationFrame {
	return contract.ActivationFrame{
		Payload:     append([]byte(nil), w.Payload...),
		Shape:       append([]uint32(nil), w.Shape...),
		DType:       contract.TensorDType(w.DType),
		Compression: contract.CompressionHint(w.Compression),
		StageIndex:  w.StageIndex,
	}
}

func taskToWire(t contract.ComputeTask) computeTaskWire {
	w := computeTaskWire{
		TaskID: t.TaskID, Component: t.Component, ResultCap: t.ResultCap,
		PipelineStage: t.PipelineStage,
		Shard: shardWire{
			Kind: uint8(t.Shard.Kind), LayerLo: t.Shard.LayerLo, LayerHi: t.Shard.LayerHi,
			TPRank: t.Shard.TPRank, TPWorld: t.Shard.TPWorld,
		},
	}
	if len(t.Caps) > 0 {
		w.Caps = append(w.Caps, t.Caps[0])
	}
	if len(t.Caps) > 1 {
		w.Input = append([]byte(nil), t.Caps[1]...)
	}
	if len(t.Deps) > 0 {
		w.Deps = append([]byte(nil), t.Deps[0].PromiseID...)
	}
	if len(t.Activation.Payload) > 0 || len(t.Activation.Shape) > 0 {
		w.Activation = activationToWire(t.Activation)
	}
	return w
}

func (w computeTaskWire) toTask() contract.ComputeTask {
	t := contract.ComputeTask{
		TaskID: w.TaskID, Component: w.Component, ResultCap: w.ResultCap,
		PipelineStage: w.PipelineStage,
		Shard: contract.Shard{
			Kind: contract.ShardKind(w.Shard.Kind), LayerLo: w.Shard.LayerLo, LayerHi: w.Shard.LayerHi,
			TPRank: w.Shard.TPRank, TPWorld: w.Shard.TPWorld,
		},
	}
	if len(w.Caps) > 0 {
		t.Caps = append(t.Caps, w.Caps[0])
	}
	if len(w.Input) > 0 {
		t.Caps = append(t.Caps, w.Input)
	}
	if len(w.Deps) > 0 {
		t.Deps = []contract.Promise{{PromiseID: append([]byte(nil), w.Deps...)}}
	}
	if len(w.Activation.Payload) > 0 || len(w.Activation.Shape) > 0 {
		t.Activation = w.Activation.toActivation()
	}
	return t
}

// computeResponse is the on-wire result frame.
type computeResponse struct {
	TaskID []byte `json:"task_id"`
	OK     bool   `json:"ok"`
	Output []byte `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

// ServeCompute registers the worker-side handler for compute dispatch. It must
// be called once, before remote requests arrive. The handler runs on a fresh
// goroutine per inbound stream.
func (f *Fabric) ServeCompute(h ComputeHandler) {
	f.host.SetStreamHandler(computeProto, func(s network.Stream) {
		f.handleComputeStream(s, h)
	})
}

func (f *Fabric) handleComputeStream(s network.Stream, h ComputeHandler) {
	ss := newStreamSession(s)
	defer func() { _ = ss.Close() }()

	raw, err := ss.Recv()
	if err != nil {
		_ = s.Reset()
		return
	}
	var req computeRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		_ = writeComputeError(ss, nil, fmt.Sprintf("decode request: %v", err))
		return
	}

	res, herr := h(f.ctx, req.Task.toTask(), req.Cap)
	if herr != nil {
		_ = writeComputeError(ss, req.Task.TaskID, herr.Error())
		return
	}
	_ = ss.Send(mustMarshalComputeResponse(computeResponse{
		TaskID: res.TaskID,
		OK:     res.OK,
		Output: res.Output,
		Error:  res.Error,
	}))
}

// ServeComputeSigned registers a worker-side compute handler that ENFORCES a
// signed capability envelope on every inbound task. The worker verifies the
// envelope in task.Caps[capSlot] against the issuer public key resolveIssuer
// yields for the grant's issuer PeerID — the key exchanged at discovery — with
// auth.Verify, at time now(), consulting isRevoked. Only a valid signature +
// live validity window + un-revoked id lets the task reach h; any failure is
// denied here, BEFORE the handler (and therefore before any wasm runs).
//
// This is the cross-kernel authority: a node now trusts a capability it did not
// mint, because it can check the issuer's signature over a key it obtained out of
// band — no shared in-process kernel and no ambient authority.
//
// now returns the current unix time (inject a fixed value in tests). isRevoked
// may be nil (signature + window only). requiredRight, when non-empty, is also
// enforced: the verified grant must convey it (e.g. contract.RightExec).
//
// wantResource is the resource THIS gate guards: the verified grant must name it
// exactly, or the task is refused before it runs. It is a parameter rather than
// something derived from f.site because there is no single site-derived answer —
// three different callers legitimately gate this one protocol on three different
// resources:
//
//	daemon/compute.WireWorker          → mesh.MeshComputeResource(site)
//	daemon/system.RegisterPipelineWorker → system.PipelineResource(site)
//	test/e2e/node                      → {KindGPU, "/cer/e2e/wasm/<node-id>"}
//
// Each matches what its own requester mints. Making the guarded resource explicit
// at the wiring site also makes "what does this gate actually protect?" a visible
// decision by whoever composes the daemon, rather than an implicit consequence of
// a config field — which is precisely how it came to be unchecked.
func (f *Fabric) ServeComputeSigned(
	h SignedComputeHandler,
	resolveIssuer IssuerPubResolver,
	now func() int64,
	isRevoked auth.RevocationPredicate,
	requiredRight contract.Right,
	wantResource contract.ResourceRef,
) {
	f.host.SetStreamHandler(computeProto, func(s network.Stream) {
		f.handleSignedComputeStream(s, h, resolveIssuer, now, isRevoked, requiredRight, wantResource)
	})
}

func (f *Fabric) handleSignedComputeStream(
	s network.Stream,
	h SignedComputeHandler,
	resolveIssuer IssuerPubResolver,
	now func() int64,
	isRevoked auth.RevocationPredicate,
	requiredRight contract.Right,
	wantResource contract.ResourceRef,
) {
	ss := newStreamSession(s)
	defer func() { _ = ss.Close() }()

	raw, err := ss.Recv()
	if err != nil {
		_ = s.Reset()
		return
	}
	var req computeRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		_ = writeComputeError(ss, nil, fmt.Sprintf("decode request: %v", err))
		return
	}
	task := req.Task.toTask()

	grant, verr := verifySignedCap(task.Caps, req.Issuer, resolveIssuer, now, isRevoked, requiredRight, wantResource, "compute")
	if verr != nil {
		// Fail closed: no bytes/work — report the denial to the requester.
		_ = writeComputeError(ss, task.TaskID, verr.Error())
		return
	}

	// Self-issued mesh compute caps (production daemon path): the grant's issuer
	// is the authenticated remote peer on this stream — mirror shard.go's binding.
	// E2E worker-granted caps name the worker as issuer while the remote peer is
	// the requester, so this check is skipped when grant.Issuer != remotePeer.
	if remotePeer, verified := ss.RemotePeerID(); verified && grant.Issuer == remotePeer {
		if req.Issuer != remotePeer {
			_ = writeComputeError(ss, task.TaskID, "mesh: compute request issuer does not match the authenticated mesh peer for this stream")
			return
		}
	}

	res, herr := h(f.ctx, task, grant)
	if herr != nil {
		_ = writeComputeError(ss, task.TaskID, herr.Error())
		return
	}
	_ = ss.Send(mustMarshalComputeResponse(computeResponse{
		TaskID: res.TaskID,
		OK:     res.OK,
		Output: res.Output,
		Error:  res.Error,
	}))
}

// verifySignedCap extracts the signed envelope from the task's cap slot, resolves
// the issuer key it names, Verifies it, and confirms it is scoped to
// wantResource. It is the single fail-closed gate the worker applies before
// running: a missing, malformed, forged, tampered, wrong-issuer, expired, or
// revoked cap — or one issued for a DIFFERENT resource — returns an error and the
// task never runs.
//
// wantResource is why this function is shared safely between compute.go and
// gpu.go. Both require RightExec, so before the scope check existed the two gates
// were interchangeable: a cap for MeshComputeResource(site) opened the GPU
// endpoint and a cap for MeshGpuResource(site) ran WASM. The right was identical;
// only the resource distinguished them, and nobody looked at it. See capscope.go.
func verifySignedCap(
	caps [][]byte,
	claimedIssuer contract.PeerID,
	resolveIssuer IssuerPubResolver,
	now func() int64,
	isRevoked auth.RevocationPredicate,
	requiredRight contract.Right,
	wantResource contract.ResourceRef,
	protocol string,
) (auth.Grant, error) {
	if resolveIssuer == nil {
		return auth.Grant{}, fmt.Errorf("mesh: no issuer resolver configured")
	}
	if len(caps) <= capSlot || len(caps[capSlot]) == 0 {
		return auth.Grant{}, fmt.Errorf("mesh: task carries no signed capability")
	}
	env := caps[capSlot]

	// The request names which issuer minted the cap; we resolve the trusted key we
	// exchanged for that issuer at discovery. auth.Verify then binds the whole
	// envelope to exactly that key AND cross-checks that the grant's own Issuer
	// field equals it — so a lie in either the frame's issuer or the grant's issuer
	// field cannot pass: the signature simply will not validate under the resolved
	// key (ErrWrongIssuer / ErrBadSignature).
	pub, ok := resolveIssuer(claimedIssuer)
	if !ok {
		return auth.Grant{}, fmt.Errorf("mesh: no trusted issuer key for %x (cap from an unknown issuer)", claimedIssuer[:8])
	}

	t := int64(0)
	if now != nil {
		t = now()
	}
	grant, err := auth.Verify(env, pub, t, isRevoked)
	if err != nil {
		return auth.Grant{}, fmt.Errorf("mesh: signed capability denied: %w", err)
	}
	if requiredRight != "" && !grantHasRight(grant, requiredRight) {
		return auth.Grant{}, fmt.Errorf("mesh: signed capability lacks required right %q", requiredRight)
	}
	if err := grantCoversResource(protocol, grant, wantResource); err != nil {
		return auth.Grant{}, err
	}
	return grant, nil
}

func grantHasRight(g auth.Grant, r contract.Right) bool {
	for _, have := range g.Rights {
		if have == r {
			return true
		}
	}
	return false
}

// RequestCompute dials a worker by its Ed25519 PeerID, sends the task and the
// authorizing capability over a QUIC stream, and returns the worker's result.
// The worker authorizes the capability before running anything.
func (f *Fabric) RequestCompute(ctx context.Context, worker contract.PeerID, task contract.ComputeTask, capH contract.CapHandle) (contract.ComputeResult, error) {
	pid, err := toLibp2pID(worker)
	if err != nil {
		return contract.ComputeResult{}, err
	}
	s, err := f.host.NewStream(ctx, pid, computeProto)
	if err != nil {
		return contract.ComputeResult{}, contract.Errf(contract.ErrPartitioned, err.Error())
	}
	ss := newStreamSession(s)
	defer func() { _ = ss.Close() }()

	body, err := json.Marshal(computeRequest{Task: taskToWire(task), Cap: capH})
	if err != nil {
		return contract.ComputeResult{}, err
	}
	if err := ss.Send(body); err != nil {
		return contract.ComputeResult{}, contract.Errf(contract.ErrPartitioned, err.Error())
	}

	raw, err := ss.Recv()
	if err != nil {
		return contract.ComputeResult{}, err
	}
	var resp computeResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return contract.ComputeResult{}, fmt.Errorf("mesh: decode compute response: %w", err)
	}
	return contract.ComputeResult{
		TaskID: resp.TaskID,
		OK:     resp.OK,
		Output: resp.Output,
		Error:  resp.Error,
	}, nil
}

// RequestComputeSigned dials a worker and dispatches a task whose authority is a
// SIGNED capability envelope carried in task.Caps[capSlot]. issuer is the PeerID
// of the node that minted+signed that envelope, so the worker can look up the
// matching public key (exchanged at discovery) and Verify. capH is still carried
// for any belt-and-suspenders in-process kernel check the worker may keep, but
// the signed envelope is the wire authority.
//
// The caller is responsible for having placed a valid envelope at
// task.Caps[capSlot] (see auth.SignedCap.Issue / IssueAttenuated).
func (f *Fabric) RequestComputeSigned(ctx context.Context, worker contract.PeerID, task contract.ComputeTask, issuer contract.PeerID, capH contract.CapHandle) (contract.ComputeResult, error) {
	pid, err := toLibp2pID(worker)
	if err != nil {
		return contract.ComputeResult{}, err
	}
	s, err := f.host.NewStream(ctx, pid, computeProto)
	if err != nil {
		return contract.ComputeResult{}, contract.Errf(contract.ErrPartitioned, err.Error())
	}
	ss := newStreamSession(s)
	defer func() { _ = ss.Close() }()

	body, err := json.Marshal(computeRequest{Task: taskToWire(task), Cap: capH, Issuer: issuer})
	if err != nil {
		return contract.ComputeResult{}, err
	}
	if err := ss.Send(body); err != nil {
		return contract.ComputeResult{}, contract.Errf(contract.ErrPartitioned, err.Error())
	}

	raw, err := ss.Recv()
	if err != nil {
		return contract.ComputeResult{}, err
	}
	var resp computeResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return contract.ComputeResult{}, fmt.Errorf("mesh: decode compute response: %w", err)
	}
	return contract.ComputeResult{
		TaskID: resp.TaskID,
		OK:     resp.OK,
		Output: resp.Output,
		Error:  resp.Error,
	}, nil
}

func writeComputeError(ss *streamSession, taskID []byte, msg string) error {
	return ss.Send(mustMarshalComputeResponse(computeResponse{TaskID: taskID, OK: false, Error: msg}))
}

func mustMarshalComputeResponse(r computeResponse) []byte {
	b, err := json.Marshal(r)
	if err != nil {
		// computeResponse only contains JSON-safe types; marshal cannot fail.
		b, _ = json.Marshal(computeResponse{OK: false, Error: "internal: marshal response"})
	}
	return b
}
