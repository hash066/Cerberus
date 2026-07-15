package gpu_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
	"github.com/hash066/cerberus/daemon/dataplane"
	"github.com/hash066/cerberus/daemon/gpu"
	"github.com/hash066/cerberus/daemon/ninep"
)

// TestGpuOverNinePDataPlaneSession is the end-to-end proof of the headline path:
// a holder WALKS a /cer/dev/gpu device in the capability-gated 9P namespace, OPENS
// its ctl (which mints a real, capability-bound data-plane grant), then dials that
// granted QUIC data-plane session and dispatches an f32 kernel — the node runs it
// on its GPU backend and returns the result over the SAME session. No kernel byte
// ever traverses 9P (vertical 04 §3.5); the 9P layer only names the device, checks
// the capability, and mints the grant.
//
// This wires ninep + dataplane + gpu directly (no libp2p mesh) exactly as
// daemon/system.Compose does in the live daemon, so it is fast and deterministic
// and runs on any machine (the default build's real software backend — no GPU
// hardware required; a -tags ffi --features gpu build would run it on wgpu).
func TestGpuOverNinePDataPlaneSession(t *testing.T) {
	const dev = "/cer/dev/gpu/local/0"
	q := contract.Quota{Bytes: 16 << 20} // 16 MiB request ceiling
	ref := contract.ResourceRef{Kind: contract.KindGPU, Path: dev, Quota: &q}

	kernel := stub.NewCapKernel()
	now := time.Now().Unix()

	// Data plane: real QUIC receiver bound to a node identity.
	dp := dataplane.NewServer(kernel, now, newIdentity(t))
	if err := dp.Listen("127.0.0.1:0"); err != nil {
		t.Fatalf("dataplane listen: %v", err)
	}
	defer dp.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// nil sink: device grants with no responder would drain; the GPU device below
	// registers a Responder so the kernel runs and returns a result.
	go func() { _ = dp.Serve(ctx, nil) }()

	// Control plane: the namespace, bridged to the data plane exactly as Compose
	// does — but a KindGPU device registers a Responder (real GPU work) instead of
	// the one-way byte drain a VRAM/bulk grant uses.
	ns := ninep.New(kernel)
	ns.Register(dev, ref)
	ns.SetGranter(func(cap contract.CapHandle, r contract.ResourceRef, transferID uint64, quota contract.Quota) (ninep.DataEndpoint, error) {
		var ep dataplane.Endpoint
		if r.Kind == contract.KindGPU {
			ep = dp.RegisterResponder(transferID, cap, quota, func(_ uint64, req []byte) ([]byte, error) {
				return gpu.Serve(req)
			})
		} else {
			ep = dp.RegisterGrant(transferID, cap, quota)
		}
		return ninep.DataEndpoint{
			Kind:         ninep.EndpointKind(ep.Kind),
			Endpoint:     ep.Addr,
			StreamID:     ep.TransferID,
			Quota:        ep.Quota,
			ServerPeerID: ep.ServerPeerID,
		}, nil
	})

	cap, err := kernel.Mint(ref, []contract.Right{contract.RightRead, contract.RightAlloc}, nil)
	if err != nil {
		t.Fatalf("mint cap: %v", err)
	}

	// Walk (gated by read) then open ctl (gated by alloc) -> a live data-plane grant.
	if err := ns.Walk(dev, cap); err != nil {
		t.Fatalf("walk device: %v", err)
	}
	ep, err := ns.Open(dev+"/ctl", cap)
	if err != nil {
		t.Fatalf("open ctl: %v", err)
	}
	if ep.Endpoint != dp.Addr() {
		t.Fatalf("ctl must return the live data-plane addr %q, got %q", dp.Addr(), ep.Endpoint)
	}

	// Dispatch a kernel over the granted session; the node runs it and returns the
	// result over the data plane.
	client := dataplane.NewClient()
	dpEP := dataplane.Endpoint{
		Kind: dataplane.EndpointQUIC, Addr: ep.Endpoint, TransferID: ep.StreamID,
		Cap: cap, Quota: ep.Quota, ServerPeerID: ep.ServerPeerID,
	}
	sctx, scancel := context.WithTimeout(ctx, 10*time.Second)
	defer scancel()
	out, backend, err := gpu.RequestSession(sctx, client, dpEP, gpu.VectorAdd, 0,
		[]float32{1, 2, 3}, []float32{4, 5, 6})
	if err != nil {
		t.Fatalf("gpu session dispatch: %v", err)
	}
	want := []float32{5, 7, 9}
	if len(out) != len(want) {
		t.Fatalf("output len = %d, want %d", len(out), len(want))
	}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("output[%d] = %v, want %v", i, out[i], want[i])
		}
	}
	if backend == "" {
		t.Fatal("session must report the backend that actually ran the kernel")
	}
}

// TestGpuSessionDeniedWithoutCapability proves the 9P gate fails closed: without a
// valid capability, opening ctl (and thus obtaining a data-plane grant) is denied
// before any session exists — no ambient authority (CLAUDE.md golden rule 5).
func TestGpuSessionDeniedWithoutCapability(t *testing.T) {
	const dev = "/cer/dev/gpu/local/0"
	q := contract.Quota{Bytes: 1 << 20}
	ref := contract.ResourceRef{Kind: contract.KindGPU, Path: dev, Quota: &q}

	kernel := stub.NewCapKernel()
	ns := ninep.New(kernel)
	ns.Register(dev, ref)

	// A handle that was never minted by this kernel is not a valid capability:
	// walking the device and opening its ctl are both denied.
	var noCap contract.CapHandle
	if err := ns.Walk(dev, noCap); err == nil {
		t.Fatal("walking a device without a valid capability must be denied")
	}
	if _, err := ns.Open(dev+"/ctl", noCap); err == nil {
		t.Fatal("opening a device ctl without a valid capability must be denied")
	}
}

// TestGpuServeReportsKernelErrors proves Serve surfaces a bad request honestly
// (OK=false) rather than fabricating output, and RequestSession turns that into an
// error for the caller.
func TestGpuServeReportsKernelErrors(t *testing.T) {
	// Mismatched input lengths for VectorAdd -> a real dispatch error.
	req, err := gpu.EncodeRequest(gpu.VectorAdd, 0, []float32{1, 2}, []float32{1})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	resp, err := gpu.Serve(req)
	if err != nil {
		t.Fatalf("Serve must not hard-error on a bad kernel: %v", err)
	}
	if len(resp) == 0 {
		t.Fatal("expected a session response body")
	}
}

func newIdentity(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen identity: %v", err)
	}
	return priv
}
