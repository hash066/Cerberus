package compute_test

import (
	"context"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
	"github.com/hash066/cerberus/daemon/compute"
	"github.com/hash066/cerberus/daemon/mesh"
	"github.com/hash066/cerberus/daemon/wasm"
	e2enode "github.com/hash066/cerberus/test/e2e/node"
)

// countingLocal records local dispatches for assertions.
type countingLocal struct {
	calls int
}

func (c *countingLocal) Dispatch(ctx context.Context, t contract.ComputeTask) (contract.PromiseHandle, error) {
	c.calls++
	return 1, nil
}

func (c *countingLocal) Resolve(ctx context.Context, p contract.PromiseHandle) (contract.ComputeResult, error) {
	return contract.ComputeResult{TaskID: []byte("local"), OK: true, Output: []byte("42")}, nil
}

func TestPreferRemoteExecutorFallsBackWithoutPeers(t *testing.T) {
	local := &countingLocal{}
	ex := compute.NewPreferRemoteExecutor(compute.PreferRemoteConfig{Local: local})
	ctx := context.Background()
	p, err := ex.Dispatch(ctx, contract.ComputeTask{TaskID: []byte("t1")})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	res, err := ex.Resolve(ctx, p)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !res.OK || string(res.Output) != "42" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if local.calls != 1 {
		t.Fatalf("expected 1 local call, got %d", local.calls)
	}
	if w := ex.LastWhere(p); w != "local" {
		t.Fatalf("expected local placement, got %q", w)
	}
}

func TestPreferRemoteExecutorDispatchesOverMesh(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	requester, err := mesh.New(ctx, mesh.Config{Site: "compute-test", Kernel: stub.NewCapKernel(), EnableMDNS: false})
	if err != nil {
		t.Fatalf("requester mesh: %v", err)
	}
	defer requester.Close()
	worker, err := mesh.New(ctx, mesh.Config{Site: "compute-test", Kernel: stub.NewCapKernel(), EnableMDNS: false})
	if err != nil {
		t.Fatalf("worker mesh: %v", err)
	}
	defer worker.Close()
	if err := requester.Connect(ctx, worker.AddrInfo()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	hello := e2enode.HelloShardWASM()
	store := wasm.NewContentStore()
	localExec := wasm.NewExecutor(hello)
	if err := compute.WireWorker(worker, compute.WorkerConfig{
		Site:      "compute-test",
		Store:     store,
		Exec:      localExec,
		SeedBytes: hello,
	}); err != nil {
		t.Fatalf("wire worker: %v", err)
	}

	local := &countingLocal{}
	ex := compute.NewPreferRemoteExecutor(compute.PreferRemoteConfig{
		Fabric: requester,
		Site:   "compute-test",
		Local:  local,
		Store:  wasm.NewContentStore(),
	})

	p, err := ex.Dispatch(ctx, contract.ComputeTask{TaskID: []byte("remote-task"), Component: hello})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	res, err := ex.Resolve(ctx, p)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !res.OK {
		t.Fatalf("remote failed: %s", res.Error)
	}
	if string(res.Output) != "1337" {
		t.Fatalf("expected hello-shard 1337, got %q", res.Output)
	}
	if local.calls != 0 {
		t.Fatalf("expected remote dispatch, local called %d times", local.calls)
	}
	if w := ex.LastWhere(p); w == "" || w == "local" {
		t.Fatalf("expected remote peer id, got %q", w)
	}
}

func TestMeshComputeResourcePath(t *testing.T) {
	r := mesh.MeshComputeResource("local")
	if r.Path != "cerberus/local/mesh-compute" {
		t.Fatalf("unexpected path: %q", r.Path)
	}
	if r.Kind != contract.KindGPU {
		t.Fatalf("unexpected kind: %v", r.Kind)
	}
}
