package mesh

// gpu_test.go covers the cross-node GPU dispatch primitive in gpu.go
// (ServeGpuSigned / RequestGpuSigned), mirroring compute_signed_test.go. The
// worker runs a deterministic in-test handler (no real GPU needed — the point is
// the capability-gated transport, not the numerics, which daemon/gpu already
// tests), so these run anywhere. They prove: the happy path round-trips a kernel +
// buffers + backend name over the real mesh; and the SAME fail-closed capability
// gate as signed compute rejects an unknown issuer, a revoked cap, and a cap
// lacking RightExec — all BEFORE the kernel handler runs.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
	"github.com/hash066/cerberus/daemon/auth"
)

// newGpuTestPair spins up two connected in-process fabrics (requester + worker)
// and returns them plus a cleanup. Shared by every gpu dispatch test.
func newGpuTestPair(t *testing.T) (requester, worker *Fabric, cleanup func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	req, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		cancel()
		t.Fatalf("requester: %v", err)
	}
	wrk, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		req.Close()
		cancel()
		t.Fatalf("worker: %v", err)
	}
	if err := req.Connect(ctx, wrk.AddrInfo()); err != nil {
		req.Close()
		wrk.Close()
		cancel()
		t.Fatalf("connect: %v", err)
	}
	return req, wrk, func() {
		req.Close()
		wrk.Close()
		cancel()
	}
}

// gpuExecCap mints a signed exec cap on a fresh issuer key for a GPU resource and
// returns the envelope, the issuer public key, and the issuer PeerID.
func gpuExecCap(t *testing.T, rights []contract.Right) (env []byte, issuerPub ed25519.PublicKey, issuerID contract.PeerID) {
	t.Helper()
	sc, pub := newTestSignedCap(t)
	copy(issuerID[:], pub)
	grant, err := auth.NewGrant(
		contract.ResourceRef{Kind: contract.KindGPU, Path: "/cer/dev/gpu/0"},
		rights, nil, time.Hour)
	if err != nil {
		t.Fatalf("new grant: %v", err)
	}
	e, err := sc.Issue(grant)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	return e, pub, issuerID
}

// TestSignedGpuRoundTrip proves the happy path: a validly signed exec cap lets the
// worker run the kernel and return the result buffer + backend name over the mesh.
func TestSignedGpuRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requester, worker, cleanup := newGpuTestPair(t)
	defer cleanup()

	env, issuerPub, issuerID := gpuExecCap(t, []contract.Right{contract.RightExec})
	resolver := func(id contract.PeerID) (ed25519.PublicKey, bool) {
		if id == issuerID {
			return issuerPub, true
		}
		return nil, false
	}

	var handlerCalls int
	worker.ServeGpuSigned(func(_ context.Context, req GpuRequest, g auth.Grant) (GpuResult, error) {
		handlerCalls++
		if g.Resource.Path != "/cer/dev/gpu/0" {
			t.Errorf("handler saw wrong resource: %+v", g.Resource)
		}
		// Deterministic vector-add so the test asserts a real value flowed back.
		out := make([]float32, len(req.A))
		for i := range req.A {
			out[i] = req.A[i] + req.B[i]
		}
		return GpuResult{Output: out, Backend: "cpu-software"}, nil
	}, resolver, func() int64 { return time.Now().Unix() }, nil)

	req := GpuRequest{KernelID: 0, A: []float32{1, 2, 3}, B: []float32{4, 5, 6}}
	res, err := requester.RequestGpuSigned(ctx, worker.PeerID(), req, issuerID, env)
	if err != nil {
		t.Fatalf("request gpu signed: %v", err)
	}
	if handlerCalls != 1 {
		t.Fatalf("handler called %d times, want 1", handlerCalls)
	}
	want := []float32{5, 7, 9}
	if len(res.Output) != len(want) {
		t.Fatalf("output len = %d, want %d", len(res.Output), len(want))
	}
	for i := range want {
		if res.Output[i] != want[i] {
			t.Fatalf("output[%d] = %v, want %v", i, res.Output[i], want[i])
		}
	}
	if res.Backend != "cpu-software" {
		t.Fatalf("backend = %q, want cpu-software", res.Backend)
	}
}

// TestSignedGpuRejectsUnknownIssuer proves the fail-closed gate: an envelope from
// an issuer the worker never met is denied before the kernel handler runs.
func TestSignedGpuRejectsUnknownIssuer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requester, worker, cleanup := newGpuTestPair(t)
	defer cleanup()

	env, _, _ := gpuExecCap(t, []contract.Right{contract.RightExec})
	resolver := func(contract.PeerID) (ed25519.PublicKey, bool) { return nil, false }

	handlerRan := false
	worker.ServeGpuSigned(func(_ context.Context, _ GpuRequest, _ auth.Grant) (GpuResult, error) {
		handlerRan = true
		return GpuResult{}, nil
	}, resolver, func() int64 { return time.Now().Unix() }, nil)

	var randomIssuer contract.PeerID
	_, _ = rand.Read(randomIssuer[:])
	_, err := requester.RequestGpuSigned(ctx, worker.PeerID(), GpuRequest{KernelID: 0, A: []float32{1}, B: []float32{2}}, randomIssuer, env)
	if err == nil {
		t.Fatal("worker ran a kernel whose issuer it has no trusted key for")
	}
	if handlerRan {
		t.Fatal("handler ran despite an unresolvable issuer — gate is not fail-closed")
	}
}

// TestSignedGpuRejectsRevokedCap proves a cryptographically-valid cap whose id the
// worker's revocation predicate flags is denied before the kernel runs.
func TestSignedGpuRejectsRevokedCap(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requester, worker, cleanup := newGpuTestPair(t)
	defer cleanup()

	env, issuerPub, issuerID := gpuExecCap(t, []contract.Right{contract.RightExec})
	verified, err := auth.Verify(env, issuerPub, time.Now().Unix(), nil)
	if err != nil {
		t.Fatalf("pre-check verify: %v", err)
	}
	resolver := func(id contract.PeerID) (ed25519.PublicKey, bool) {
		if id == issuerID {
			return issuerPub, true
		}
		return nil, false
	}
	isRevoked := func(id contract.CapID) bool { return id == verified.ID }

	handlerRan := false
	worker.ServeGpuSigned(func(_ context.Context, _ GpuRequest, _ auth.Grant) (GpuResult, error) {
		handlerRan = true
		return GpuResult{}, nil
	}, resolver, func() int64 { return time.Now().Unix() }, isRevoked)

	_, err = requester.RequestGpuSigned(ctx, worker.PeerID(), GpuRequest{KernelID: 0, A: []float32{1}, B: []float32{2}}, issuerID, env)
	if err == nil {
		t.Fatal("worker ran a kernel carrying a revoked capability")
	}
	if handlerRan {
		t.Fatal("handler ran despite the capability being revoked")
	}
}

// TestSignedGpuRejectsMissingExecRight proves a validly signed, unrevoked cap that
// conveys only RightRead is denied at an exec-gated GPU endpoint.
func TestSignedGpuRejectsMissingExecRight(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requester, worker, cleanup := newGpuTestPair(t)
	defer cleanup()

	env, issuerPub, issuerID := gpuExecCap(t, []contract.Right{contract.RightRead}) // read only
	resolver := func(id contract.PeerID) (ed25519.PublicKey, bool) {
		if id == issuerID {
			return issuerPub, true
		}
		return nil, false
	}

	handlerRan := false
	worker.ServeGpuSigned(func(_ context.Context, _ GpuRequest, _ auth.Grant) (GpuResult, error) {
		handlerRan = true
		return GpuResult{}, nil
	}, resolver, func() int64 { return time.Now().Unix() }, nil)

	_, err := requester.RequestGpuSigned(ctx, worker.PeerID(), GpuRequest{KernelID: 0, A: []float32{1}, B: []float32{2}}, issuerID, env)
	if err == nil {
		t.Fatal("worker ran a kernel whose capability lacks RightExec")
	}
	if handlerRan {
		t.Fatal("handler ran despite the capability lacking RightExec")
	}
}
