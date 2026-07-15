package gpu_test

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/gpu"
	"github.com/hash066/cerberus/daemon/mesh"
)

func TestRemoteVectorAddOnPeerGPU(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	requester, worker, cleanup := newGpuMeshPair(t, ctx)
	defer cleanup()

	a := []float32{1, 2, 3}
	b := []float32{4, 5, 6}
	out, backend, where, err := gpu.DispatchRemote(ctx, requester, "gpu-test", worker.PeerID(), gpu.VectorAdd, 0, a, b)
	if err != nil {
		t.Fatalf("remote dispatch: %v", err)
	}
	want := []float32{5, 7, 9}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("output[%d] = %v, want %v", i, out[i], want[i])
		}
	}
	if backend != "cpu-software" {
		t.Fatalf("backend = %q, want cpu-software (software path in default build)", backend)
	}
	if where == "" {
		t.Fatal("expected non-empty where")
	}
}

func TestRemoteSaxpyOnPeerGPU(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	requester, worker, cleanup := newGpuMeshPair(t, ctx)
	defer cleanup()

	x := []float32{1, 2, 3}
	y := []float32{0.5, 0.5, 0.5}
	out, _, _, err := gpu.DispatchRemote(ctx, requester, "gpu-test", worker.PeerID(), gpu.Saxpy, 2, x, y)
	if err != nil {
		t.Fatalf("remote saxpy: %v", err)
	}
	want := []float32{2.5, 4.5, 6.5}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("output[%d] = %v, want %v", i, out[i], want[i])
		}
	}
}

func TestQuotaRejectsOversizedKernel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	requester, worker, cleanup := newGpuMeshPair(t, ctx)
	defer cleanup()

	a := []float32{1, 2, 3}
	b := []float32{4, 5, 6}
	req := mesh.GpuRequest{KernelID: int(gpu.VectorAdd), A: a, B: b}
	env, issuerID, err := mintTinyGpuCap(requester, "gpu-test")
	if err != nil {
		t.Fatalf("mint cap: %v", err)
	}
	_, err = requester.RequestGpuSigned(ctx, worker.PeerID(), req, issuerID, env)
	if err == nil {
		t.Fatal("expected quota denial for oversized kernel")
	}
}

func newGpuMeshPair(t *testing.T, ctx context.Context) (requester, worker *mesh.Fabric, cleanup func()) {
	t.Helper()
	requester, err := mesh.New(ctx, mesh.Config{Site: "gpu-test", Kernel: stub.NewCapKernel(), EnableMDNS: false})
	if err != nil {
		t.Fatalf("requester: %v", err)
	}
	worker, werr := mesh.New(ctx, mesh.Config{Site: "gpu-test", Kernel: stub.NewCapKernel(), EnableMDNS: false})
	if werr != nil {
		requester.Close()
		t.Fatalf("worker: %v", err)
	}
	if err := requester.Connect(ctx, worker.AddrInfo()); err != nil {
		requester.Close()
		worker.Close()
		t.Fatalf("connect: %v", err)
	}
	if err := gpu.WireWorker(worker, gpu.WorkerConfig{Site: "gpu-test"}); err != nil {
		requester.Close()
		worker.Close()
		t.Fatalf("wire worker: %v", err)
	}
	return requester, worker, func() {
		requester.Close()
		worker.Close()
	}
}

func mintTinyGpuCap(fab *mesh.Fabric, site string) ([]byte, contract.PeerID, error) {
	identity := fab.Identity()
	if len(identity) != ed25519.PrivateKeySize {
		return nil, contract.PeerID{}, nil
	}
	ks, err := auth.NewMemoryKeyStore(identity.Seed())
	if err != nil {
		return nil, contract.PeerID{}, err
	}
	signer := auth.NewSignedCap(ks)
	res := mesh.MeshGpuResource(site)
	res.Quota = &contract.Quota{Bytes: 1, Flops: 1}
	g, err := auth.NewGrant(
		res,
		[]contract.Right{contract.RightExec},
		nil,
		time.Hour,
	)
	if err != nil {
		return nil, contract.PeerID{}, err
	}
	env, err := signer.Issue(g)
	if err != nil {
		return nil, contract.PeerID{}, err
	}
	issuerID, err := signer.IssuerPeerID()
	if err != nil {
		return nil, contract.PeerID{}, err
	}
	return env, issuerID, nil
}
