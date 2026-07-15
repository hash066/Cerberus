package system

import (
	"bytes"
	"context"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/inference"
	"github.com/hash066/cerberus/daemon/mesh"
)

// TestPipelineRunnerMLXBackend forces mock mode so the test is deterministic
// on every platform: the runner must report "mlx-mock" (never a fake "mlx")
// and the shard-chained output must equal a single full-range forward.
func TestPipelineRunnerMLXBackend(t *testing.T) {
	t.Setenv("CERBERUS_MLX_MOCK", "1")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	kernel := stub.NewCapKernel()
	sys, err := Compose(ctx, kernel, "pipeline-mlx", nil)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	defer stopSystem(t, sys, cancel)

	fab, ok := sys.Fabric.(*mesh.Fabric)
	if !ok {
		t.Fatal("expected *mesh.Fabric")
	}

	runner, err := NewPipelineRunner(sys, fab, nil)
	if err != nil {
		t.Fatalf("NewPipelineRunner: %v", err)
	}
	runner.Backend = inference.BackendMLX

	result, err := runner.RunPipeline(ctx, []byte("mlx-local"), DefaultSplitMLPShards(), nil)
	if err != nil {
		t.Fatalf("RunPipeline: %v", err)
	}
	if !result.OK {
		t.Fatalf("pipeline failed: %s", result.Error)
	}
	if result.Backend != inference.ReportedBackend(inference.BackendMLX) {
		t.Fatalf("backend = %q", result.Backend)
	}
	want, _, err := inference.ForwardRange(inference.BackendMLX, EncodeActivation(SplitMLPDefaultInput), 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(result.Output, want) {
		t.Fatalf("output mismatch")
	}
	if len(result.Stages) != 2 {
		t.Fatalf("expected 2 stages, got %d", len(result.Stages))
	}
}

// TestPipelineWorkerMLXComponent: a worker receiving an mlx-tagged shard task
// must route it through the mlx backend (mock or real per its own platform).
func TestPipelineWorkerMLXComponent(t *testing.T) {
	t.Setenv("CERBERUS_MLX_MOCK", "1")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	svc := &PipelineWorker{mailbox: newActivationMailbox(), site: "t"}
	task := contract.ComputeTask{
		TaskID:    []byte("t"),
		Component: inference.ComponentTag(inference.BackendMLX),
		Shard:     contract.Shard{Kind: contract.ShardPipeline, LayerLo: 0, LayerHi: 1},
	}
	res, err := svc.handlePipelineCompute(ctx, task, auth.Grant{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatal(res.Error)
	}
	want, _, err := inference.ForwardRange(inference.BackendMLX, EncodeActivation(SplitMLPDefaultInput), 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(res.Output, want) {
		t.Fatal("worker mlx shard output mismatch")
	}
}
