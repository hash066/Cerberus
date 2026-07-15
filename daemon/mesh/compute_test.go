package mesh

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
)

// TestRequestComputeOverMesh spins up two real libp2p/QUIC fabric nodes, connects
// them, and runs a capability-gated compute round-trip: the requester sends a
// ComputeTask, the worker verifies the presented cap against its own kernel and
// returns a result. This is the transport the e2e demo dispatches over.
func TestRequestComputeOverMesh(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	reqKernel := stub.NewCapKernel()
	wrkKernel := stub.NewCapKernel()

	requester, err := New(ctx, Config{Site: "test", Kernel: reqKernel})
	if err != nil {
		t.Fatalf("requester: %v", err)
	}
	defer requester.Close()
	worker, err := New(ctx, Config{Site: "test", Kernel: wrkKernel})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	defer worker.Close()

	if err := requester.Connect(ctx, worker.AddrInfo()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// Worker authorizes against its OWN kernel, then echoes a result.
	worker.ServeCompute(func(_ context.Context, task contract.ComputeTask, capH contract.CapHandle) (contract.ComputeResult, error) {
		if err := wrkKernel.Verify(capH, contract.Request{Op: "exec"}, time.Now().Unix()); err != nil {
			return contract.ComputeResult{TaskID: task.TaskID, OK: false, Error: err.Error()}, nil
		}
		return contract.ComputeResult{TaskID: task.TaskID, OK: true, Output: []byte("42")}, nil
	})

	// Worker grants the requester an exec cap (resource owner grants authority).
	grant, err := wrkKernel.Mint(contract.ResourceRef{Kind: contract.KindGPU}, []contract.Right{contract.RightExec}, nil)
	if err != nil {
		t.Fatalf("mint grant: %v", err)
	}

	task := contract.ComputeTask{TaskID: []byte("t1"), Component: []byte("cid-bytes")}
	res, err := requester.RequestCompute(ctx, worker.PeerID(), task, grant)
	if err != nil {
		t.Fatalf("request compute: %v", err)
	}
	if !res.OK || string(res.Output) != "42" {
		t.Fatalf("unexpected result ok=%v out=%q err=%q", res.OK, res.Output, res.Error)
	}
}

// TestRequestComputeDeniedWithoutCap proves the gate is real: a task presenting a
// capability the worker's kernel does not recognize (here, the zero handle) is
// rejected by the worker, not executed.
func TestRequestComputeDeniedWithoutCap(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	wrkKernel := stub.NewCapKernel()
	requester, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("requester: %v", err)
	}
	defer requester.Close()
	worker, err := New(ctx, Config{Site: "test", Kernel: wrkKernel})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	defer worker.Close()

	if err := requester.Connect(ctx, worker.AddrInfo()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	worker.ServeCompute(func(_ context.Context, task contract.ComputeTask, capH contract.CapHandle) (contract.ComputeResult, error) {
		if err := wrkKernel.Verify(capH, contract.Request{Op: "exec"}, time.Now().Unix()); err != nil {
			return contract.ComputeResult{TaskID: task.TaskID, OK: false, Error: "denied"}, nil
		}
		return contract.ComputeResult{TaskID: task.TaskID, OK: true, Output: []byte("ran")}, nil
	})

	res, err := requester.RequestCompute(ctx, worker.PeerID(), contract.ComputeTask{TaskID: []byte("t")}, contract.CapHandle(0))
	if err != nil {
		t.Fatalf("request compute: %v", err)
	}
	if res.OK {
		t.Fatal("worker executed a task with no valid capability")
	}
}

func TestComputeTaskWireRoundTripPreservesShard(t *testing.T) {
	task := contract.ComputeTask{
		TaskID:    []byte("pipe"),
		Component: []byte("splitmlp/go-fixture"),
		Shard:     contract.Shard{Kind: contract.ShardPipeline, LayerLo: 2, LayerHi: 3},
		Caps:      [][]byte{[]byte("signed-cap"), []byte("activation")},
	}
	w := taskToWire(task)
	raw, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	var decoded computeTaskWire
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	back := decoded.toTask()
	if back.Shard.LayerLo != 2 || back.Shard.LayerHi != 3 {
		t.Fatalf("shard not preserved: got [%d,%d]", back.Shard.LayerLo, back.Shard.LayerHi)
	}
	if len(back.Caps) != 2 || string(back.Caps[1]) != "activation" {
		t.Fatalf("caps/input not preserved: %+v", back.Caps)
	}
}
