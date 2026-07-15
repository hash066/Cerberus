package system

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
	"github.com/hash066/cerberus/daemon/mesh"
	"github.com/hash066/cerberus/daemon/scheduler"
)

func TestInferenceServiceSplitMLP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	kernel := stub.NewCapKernel()
	sys, err := Compose(ctx, kernel, "inf-test", nil)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	defer stopSystem(t, sys, cancel)

	fab, ok := sys.Fabric.(*mesh.Fabric)
	if !ok {
		t.Fatal("expected mesh fabric")
	}
	runner, err := NewPipelineRunner(sys, fab, nil)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewInferenceService(runner, PipelineFixtureModels())

	res, err := svc.Run(ctx, "alice", SplitMLPDemoModel.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "split-mlp") {
		t.Fatalf("unexpected content: %q", res.Content)
	}
	if len(res.Stages) != 2 {
		t.Fatalf("expected 2 stages, got %d", len(res.Stages))
	}
}

func TestInferenceServiceStreamEmitsStageTokens(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	kernel := stub.NewCapKernel()
	fab, err := mesh.New(ctx, mesh.Config{Site: "inf", Kernel: kernel, EnableMDNS: false})
	if err != nil {
		t.Fatal(err)
	}
	defer fab.Close()

	sched := scheduler.New(nil)
	sched.UpdateNode(localTelemetry(fab.PeerID()))
	runner, err := NewPipelineRunnerFromFabric(fab, sched, nil)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewInferenceService(runner, PipelineFixtureModels())

	var tokens []string
	_, err = svc.RunStream(ctx, "bob", SplitMLPDemoModel.ID, nil, func(tok string) error {
		tokens = append(tokens, tok)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) < 3 {
		t.Fatalf("expected stage + final tokens, got %d: %v", len(tokens), tokens)
	}
	if !strings.Contains(tokens[0], "stage L0-1") {
		t.Fatalf("first token should be stage 0-1, got %q", tokens[0])
	}
}

func TestShardsForLayerCountTwoNodes(t *testing.T) {
	shards, err := ShardsForLayerCount(4, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(shards) != 2 {
		t.Fatalf("got %d shards", len(shards))
	}
	want := DefaultSplitMLPShards()
	for i := range shards {
		if shards[i] != want[i] {
			t.Fatalf("shard %d: got %+v want %+v", i, shards[i], want[i])
		}
	}

	shards, err = ShardsForLayerCount(22, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(shards) != 2 {
		t.Fatal(shards)
	}
	if shards[0].LayerLo != 0 || shards[0].LayerHi != 10 {
		t.Fatalf("first shard: %+v", shards[0])
	}
	if shards[1].LayerLo != 11 || shards[1].LayerHi != 21 {
		t.Fatalf("second shard: %+v", shards[1])
	}
	for _, sh := range shards {
		if sh.Kind != contract.ShardPipeline {
			t.Fatalf("kind = %v", sh.Kind)
		}
	}
}

// TestShardsForSpecCoversEveryLayer exercises the Phase-2 layer-split substrate
// with a synthetic 22-layer spec. It is NOT tied to any real model: llama.cpp owns
// the layer split on the real path (daemon/llama), so this function is not on it.
func TestShardsForSpecCoversEveryLayer(t *testing.T) {
	spec := InferenceModelSpec{ID: "synthetic-22L", Backend: InferenceBackendCPUSoftware, LayerCount: 22}
	shards, err := shardsForSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(shards) != DefaultPipelineNodeCount {
		t.Fatalf("got %d shards want %d", len(shards), DefaultPipelineNodeCount)
	}
	total := int(shards[len(shards)-1].LayerHi + 1)
	if total != int(spec.LayerCount) {
		t.Fatalf("layer coverage = %d want %d", total, spec.LayerCount)
	}
}

// TestBuiltinInferenceModelsExposesNoFixtureOrMock is a REGRESSION GUARD for the
// L0 cleanup, and it is the inverse of the test that used to live here.
//
// The old test asserted that /v1/models advertised "llamacpp-mock", "tinyllama-1b"
// and "llama-3.2-1b". None of those ran a real model: the first was a mock
// transform, the second additionally prompted llama-cli with the auth subject, the
// third needed a sidecar that rejected models above 4 layers. The test passing was
// what made the lie look maintained.
//
// /v1/models must advertise only models that genuinely run. Real entries come from
// daemon/llama. The split-MLP fixture must never appear here.
func TestBuiltinInferenceModelsExposesNoFixtureOrMock(t *testing.T) {
	if got := BuiltinInferenceModels(); len(got) != 0 {
		t.Fatalf("BuiltinInferenceModels() = %+v, want empty — only genuinely-running models may reach /v1/models", got)
	}
}

// TestPipelineFixtureModelsStillCarriesTheFixture pins the other half: the fixture
// remains reachable for `cerberus pipeline-run` and the e2e harnesses, which is
// honest, because nothing there claims it is a language model.
func TestPipelineFixtureModelsStillCarriesTheFixture(t *testing.T) {
	got := PipelineFixtureModels()
	if len(got) != 1 || got[0].ID != SplitMLPDemoModel.ID {
		t.Fatalf("PipelineFixtureModels() = %+v, want just %q", got, SplitMLPDemoModel.ID)
	}
	if got[0].Fixture != InferenceFixtureSplitMLP {
		t.Fatalf("fixture = %q, want %q", got[0].Fixture, InferenceFixtureSplitMLP)
	}
}
