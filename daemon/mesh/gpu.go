package mesh

// gpu.go is a capability-gated request/response for CROSS-NODE GPU dispatch over
// a point-to-point libp2p/QUIC stream. It is the GPU-kernel sibling of compute.go:
// where compute.go ships a WASM ComputeTask, this ships a small element-wise f32
// GPU kernel + input buffers, the worker authorizes a SIGNED capability BEFORE any
// work runs, executes the kernel on ITS OWN best available backend (the physical
// GPU via wgpu under `-tags ffi --features gpu`, else a real software backend),
// and returns the result buffer + the name of the backend that actually ran.
//
// What is REAL here:
//   - The transport is the existing libp2p host over QUIC (quic-v1): encrypted,
//     multiplexed, PeerID-authenticated by the QUIC/TLS handshake, reusing the
//     same streamSession 4-byte length framing as compute.go.
//   - The authority is a CROSS-KERNEL SIGNED capability (daemon/auth.SignedCap,
//     Ed25519-signed by the granting node's issuer key). The worker VERIFIES it
//     against the granting node's issuer PUBLIC KEY — exchanged out of band at
//     discovery — via the SAME verifySignedCap gate compute.go uses, BEFORE the
//     kernel runs. No ambient authority, no shared in-process kernel: a node runs
//     a peer's kernel only on a capability it can cryptographically check. The
//     required right is RightExec (running compute), matching the compute path.
//   - The kernel itself is run by a caller-supplied GpuHandler. This package
//     intentionally does NOT import daemon/gpu: the daemon composition wires the
//     handler to gpu.Dispatch, keeping the mesh lane transport-only and free of a
//     GPU-backend dependency (and letting a test inject a deterministic handler).
//
// HONEST SCOPE (not faked): this file is the tested mesh-layer PRIMITIVE for
// cross-node GPU dispatch. Wiring it into the composed daemon's `cerberus gpu
// --on <peer>` path is the documented next step (see docs/gpu.md "Cross-node GPU
// dispatch"): the shipping daemon does not yet exchange issuer trust anchors at
// discovery the way test/e2e/node does, which is the prerequisite for a composed
// end-to-end run. The primitive here is complete and unit-tested on its own.

import (
	"context"
	"encoding/json"
	"fmt"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/libp2p/go-libp2p/core/network"
)

// gpuProto is the libp2p protocol id for capability-gated cross-node GPU dispatch.
const gpuProto = "/cerberus/gpu/1.0.0"

// MeshGpuResource returns the per-site resource a mesh GPU dispatch capability
// must name: contract.KindGPU at "cerberus/<site>/mesh-gpu". Production daemons
// mint a signed RightExec capability against this resource (self-issued under the
// mesh identity key, stream-bound — see shard.go's issuer trust model) before
// calling RequestGpuSigned.
func MeshGpuResource(site string) contract.ResourceRef {
	return contract.ResourceRef{Kind: contract.KindGPU, Path: "cerberus/" + site + "/mesh-gpu"}
}

// GpuRequest is one element-wise f32 GPU kernel to run on a peer. KernelID mirrors
// the daemon/gpu + core/cabi ABI (0=VectorAdd, 1=Saxpy(alpha=Param), 2=ScalarMul
// (scalar=Param); B is ignored for ScalarMul). It is the argument a verified
// GpuHandler receives — the transport has already authorized the caller by the
// time the handler sees this.
type GpuRequest struct {
	KernelID int
	Param    float32
	A        []float32
	B        []float32
}

// GpuResult is a peer's answer: the output buffer and the backend that ACTUALLY
// ran it ("gpu-wgpu" or "cpu-software"/…). Backend is surfaced verbatim so the
// requester can report exactly where its kernel ran (maturity honesty: a peer
// that fell back to software says so).
type GpuResult struct {
	Output  []float32
	Backend string
}

// GpuHandler runs an already-authorized GPU request on the worker side. It is
// invoked ONLY AFTER the signed capability envelope has been verified (signature +
// validity window + revocation + RightExec), and receives the verified auth.Grant
// so it can enforce the grant's resource scope if it wishes. Returning an error
// aborts the stream with that message; otherwise the GpuResult is sent back
// verbatim. The daemon wires this to daemon/gpu.Dispatch.
type GpuHandler func(ctx context.Context, req GpuRequest, grant auth.Grant) (GpuResult, error)

// gpuRequestWire is the on-wire request frame: the kernel request, the signed
// capability envelope that authorizes it (in Caps[capSlot], matching compute.go's
// slotting), and the Issuer hint naming which node minted the envelope so the
// worker can resolve the matching public key. JSON keeps it inspectable and
// dependency-free, exactly like computeRequest.
type gpuRequestWire struct {
	Req    GpuRequest      `json:"req"`
	Caps   [][]byte        `json:"caps,omitempty"`
	Issuer contract.PeerID `json:"issuer,omitempty"`
}

// gpuResponseWire is the on-wire result frame.
type gpuResponseWire struct {
	OK      bool      `json:"ok"`
	Output  []float32 `json:"output,omitempty"`
	Backend string    `json:"backend,omitempty"`
	Error   string    `json:"error,omitempty"`
}

// ServeGpuSigned registers the worker-side handler for cross-node GPU dispatch,
// ENFORCING a signed capability on every inbound request. The worker verifies the
// envelope in Caps[capSlot] against the issuer public key resolveIssuer yields for
// the request's Issuer PeerID (the key exchanged at discovery), at time now(),
// consulting isRevoked, and requiring RightExec — the SAME fail-closed gate the
// signed compute path uses. Only a valid signature + live window + un-revoked id +
// RightExec lets the request reach h; any failure is denied here, before h runs.
//
// now returns the current unix time (inject a fixed value in tests). isRevoked may
// be nil (signature + window only).
func (f *Fabric) ServeGpuSigned(
	h GpuHandler,
	resolveIssuer IssuerPubResolver,
	now func() int64,
	isRevoked auth.RevocationPredicate,
) {
	f.host.SetStreamHandler(gpuProto, func(s network.Stream) {
		f.handleSignedGpuStream(s, h, resolveIssuer, now, isRevoked)
	})
}

func (f *Fabric) handleSignedGpuStream(
	s network.Stream,
	h GpuHandler,
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
	var req gpuRequestWire
	if err := json.Unmarshal(raw, &req); err != nil {
		_ = writeGpuError(ss, fmt.Sprintf("decode request: %v", err))
		return
	}

	// Same fail-closed capability gate as the signed compute path: a missing,
	// malformed, forged, tampered, wrong-issuer, expired, or revoked cap — or one
	// that does not convey RightExec — returns an error and the kernel never runs.
	grant, verr := verifySignedCap(req.Caps, req.Issuer, resolveIssuer, now, isRevoked, contract.RightExec)
	if verr != nil {
		_ = writeGpuError(ss, verr.Error())
		return
	}

	res, herr := h(f.ctx, req.Req, grant)
	if herr != nil {
		_ = writeGpuError(ss, herr.Error())
		return
	}
	_ = ss.Send(mustMarshalGpuResponse(gpuResponseWire{
		OK:      true,
		Output:  res.Output,
		Backend: res.Backend,
	}))
}

// RequestGpuSigned dials a worker and dispatches a GPU kernel whose authority is a
// SIGNED capability envelope carried in capEnvelope. issuer is the PeerID of the
// node that minted+signed that envelope, so the worker can look up the matching
// public key (exchanged at discovery) and Verify it. The worker runs the kernel on
// its own best backend and returns the result plus the backend name.
//
// The caller is responsible for having obtained a valid envelope authorizing
// RightExec on the worker's GPU resource (see auth.SignedCap.Issue).
func (f *Fabric) RequestGpuSigned(ctx context.Context, worker contract.PeerID, req GpuRequest, issuer contract.PeerID, capEnvelope []byte) (GpuResult, error) {
	pid, err := toLibp2pID(worker)
	if err != nil {
		return GpuResult{}, err
	}
	s, err := f.host.NewStream(ctx, pid, gpuProto)
	if err != nil {
		return GpuResult{}, contract.Errf(contract.ErrPartitioned, err.Error())
	}
	ss := newStreamSession(s)
	defer func() { _ = ss.Close() }()

	body, err := json.Marshal(gpuRequestWire{
		Req:    req,
		Caps:   [][]byte{capEnvelope},
		Issuer: issuer,
	})
	if err != nil {
		return GpuResult{}, err
	}
	if err := ss.Send(body); err != nil {
		return GpuResult{}, contract.Errf(contract.ErrPartitioned, err.Error())
	}

	raw, err := ss.Recv()
	if err != nil {
		return GpuResult{}, err
	}
	var resp gpuResponseWire
	if err := json.Unmarshal(raw, &resp); err != nil {
		return GpuResult{}, fmt.Errorf("mesh: decode gpu response: %w", err)
	}
	if !resp.OK {
		// The worker denied or failed the request; surface its reason, not a
		// fabricated result.
		return GpuResult{}, fmt.Errorf("mesh: peer gpu dispatch denied/failed: %s", resp.Error)
	}
	return GpuResult{Output: resp.Output, Backend: resp.Backend}, nil
}

func writeGpuError(ss *streamSession, msg string) error {
	return ss.Send(mustMarshalGpuResponse(gpuResponseWire{OK: false, Error: msg}))
}

func mustMarshalGpuResponse(r gpuResponseWire) []byte {
	b, err := json.Marshal(r)
	if err != nil {
		// gpuResponseWire only holds JSON-safe types; marshal cannot fail.
		b, _ = json.Marshal(gpuResponseWire{OK: false, Error: "internal: marshal response"})
	}
	return b
}
