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
	svc := NewInferenceService(runner, BuiltinInferenceModels())

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
	svc := NewInferenceService(runner, BuiltinInferenceModels())

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

func TestShardsForSpecTinyLlama(t *testing.T) {
	shards, err := shardsForSpec(TinyLlamaModel)
	if err != nil {
		t.Fatal(err)
	}
	if len(shards) != 2 {
		t.Fatalf("got %d shards", len(shards))
	}
	total := int(shards[len(shards)-1].LayerHi + 1)
	if total != int(TinyLlamaModel.LayerCount) {
		t.Fatalf("layer coverage = %d want %d", total, TinyLlamaModel.LayerCount)
	}
}

func TestBuiltinInferenceModelsIncludeLLMEntries(t *testing.T) {
	ids := map[string]bool{}
	for _, m := range BuiltinInferenceModels() {
		ids[m.ID] = true
	}
	for _, id := range []string{"split-mlp-demo", "llamacpp-mock", "tinyllama-1b", "llama-3.2-1b"} {
		if !ids[id] {
			t.Fatalf("missing model %q", id)
		}
	}
}
