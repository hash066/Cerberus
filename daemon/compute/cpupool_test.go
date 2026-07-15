package compute_test

import (
	"context"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
	"github.com/hash066/cerberus/daemon/compute"
	"github.com/hash066/cerberus/daemon/mesh"
	"github.com/hash066/cerberus/daemon/scheduler"
	"github.com/hash066/cerberus/daemon/wasm"
	e2enode "github.com/hash066/cerberus/test/e2e/node"
)

func TestPreferRemoteExecutorDispatchesWhenLocalSaturated(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	requester, err := mesh.New(ctx, mesh.Config{Site: "cpu-pool-test", Kernel: stub.NewCapKernel(), EnableMDNS: false})
	if err != nil {
		t.Fatalf("requester mesh: %v", err)
	}
	defer requester.Close()
	worker, err := mesh.New(ctx, mesh.Config{Site: "cpu-pool-test", Kernel: stub.NewCapKernel(), EnableMDNS: false})
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

	workerSched := scheduler.New(nil)
	if err := compute.WireWorker(worker, compute.WorkerConfig{
		Site: "cpu-pool-test", Store: store, Exec: localExec, SeedBytes: hello, Sched: workerSched,
	}); err != nil {
		t.Fatalf("wire worker: %v", err)
	}

	sched := scheduler.New(nil)
	self := requester.PeerID()
	workerID := worker.PeerID()
	tel := func(id contract.PeerID, cores uint32) contract.NodeTelemetry {
		return contract.NodeTelemetry{
			PeerID: id, Compute: contract.Compute{PCores: cores},
			Memory: contract.Memory{VRAMFree: 8_000_000_000}, Thermal: contract.Thermal{HeadroomC: 20},
			Power: contract.Power{Src: contract.PowerAC},
		}
	}
	sched.UpdateNode(tel(self, 2))
	sched.UpdateNode(tel(workerID, 8))
	sched.AcquireCPU(self, 2) // saturate requester locally

	local := &countingLocal{}
	ex := compute.NewPreferRemoteExecutor(compute.PreferRemoteConfig{
		Fabric: requester, Site: "cpu-pool-test", Local: local, Store: wasm.NewContentStore(), Sched: sched,
	})

	p, err := ex.Dispatch(ctx, contract.ComputeTask{TaskID: []byte("cpu-pool"), Component: hello})
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
	if local.calls != 0 {
		t.Fatalf("expected remote dispatch when local saturated, local called %d times", local.calls)
	}
	where := ex.LastWhere(p)
	if where == "" || where == "local" {
		t.Fatalf("expected remote peer placement, got %q", where)
	}
}

func TestPreferRemoteExecutorUsesLocalWhenMostFree(t *testing.T) {
	sched := scheduler.New(nil)
	var self contract.PeerID
	self[0] = 0xAA
	sched.UpdateNode(contract.NodeTelemetry{
		PeerID: self, Compute: contract.Compute{PCores: 8},
		Memory: contract.Memory{VRAMFree: 8_000_000_000}, Thermal: contract.Thermal{HeadroomC: 20},
		Power: contract.Power{Src: contract.PowerAC},
	})
	var peer contract.PeerID
	peer[0] = 0xBB
	sched.UpdateNode(contract.NodeTelemetry{
		PeerID: peer, Compute: contract.Compute{PCores: 2},
		Memory: contract.Memory{VRAMFree: 8_000_000_000}, Thermal: contract.Thermal{HeadroomC: 20},
		Power: contract.Power{Src: contract.PowerAC},
	})

	local := &countingLocal{}
	ex := compute.NewPreferRemoteExecutor(compute.PreferRemoteConfig{Local: local, Sched: sched})

	best, free, ok := sched.BestNode(nil)
	if !ok || best != self || free != 8 {
		t.Fatalf("scheduler should prefer self with 8 free cores, got %v free=%d ok=%v", best[0], free, ok)
	}

	ctx := context.Background()
	p, err := ex.Dispatch(ctx, contract.ComputeTask{TaskID: []byte("local-pick")})
	if err != nil {
		t.Fatal(err)
	}
	if ex.LastWhere(p) != "local" {
		t.Fatalf("expected local dispatch, got %q", ex.LastWhere(p))
	}
}
