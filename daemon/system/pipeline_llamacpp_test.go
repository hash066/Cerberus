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

func TestPipelineRunnerLlamacppMockBackend(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	kernel := stub.NewCapKernel()
	sys, err := Compose(ctx, kernel, "pipeline-llamacpp", nil)
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
	runner.Backend = inference.BackendLlamacpp

	result, err := runner.RunPipeline(ctx, []byte("llamacpp-local"), DefaultSplitMLPShards(), nil)
	if err != nil {
		t.Fatalf("RunPipeline: %v", err)
	}
	if !result.OK {
		t.Fatalf("pipeline failed: %s", result.Error)
	}
	if result.Backend != inference.ReportedBackend(inference.BackendLlamacpp) {
		t.Fatalf("backend = %q", result.Backend)
	}
	want, _, err := inference.ForwardRange(inference.BackendLlamacpp, EncodeActivation(SplitMLPDefaultInput), 0, 3)
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

func TestPipelineWorkerLlamacppComponent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	svc := &PipelineWorker{mailbox: newActivationMailbox(), site: "t"}
	task := contract.ComputeTask{
		TaskID:    []byte("t"),
		Component: inference.ComponentTag(inference.BackendLlamacpp),
		Shard:     contract.Shard{Kind: contract.ShardPipeline, LayerLo: 0, LayerHi: 1},
	}
	res, err := svc.handlePipelineCompute(ctx, task, auth.Grant{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatal(res.Error)
	}
}
