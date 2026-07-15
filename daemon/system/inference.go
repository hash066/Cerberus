// inference.go is the entry point for the PIPELINE FIXTURE path: it drives the
// deterministic split-MLP activation across mesh nodes through PipelineRunner.
//
// WHAT THIS IS: a test/demo harness for Cerberus's own distribution machinery
// (mesh streams, dataplane, scheduler, signed capability gates). The split-MLP
// fixture is 4 layers x 4 dimensions with weights from a formula. It is not a
// language model and has no tokenizer, weights, KV-cache or sampler.
//
// WHAT THIS IS NOT: real LLM inference. That lives in daemon/llama, which
// supervises upstream llama.cpp and tunnels its RPC over a capability-gated mesh
// session. Chat completions are served from there, not from here.
//
// DELIBERATELY NOT ON /v1/models: BuiltinInferenceModels() is empty. The fixture is
// reachable from `cerberus pipeline-run` and the e2e harnesses, which is honest —
// listing it as a chat model on /v1/models would not be. v0.1 shipped
// `llamacpp-mock`, `tinyllama-1b` and `llama-3.2-1b` here; none of them ran a real
// model and all three were removed in Lane L / L0.
package system

import (
	"context"
	"fmt"
	"strings"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/inference"
)

// InferenceBackend names the compute backend for an inference model. Only the
// split-MLP fixture backend exists; the llama.cpp / MLX entries were mocks and are
// gone (see daemon/inference/backend.go).
type InferenceBackend string

const (
	InferenceBackendCPUSoftware InferenceBackend = "cpu-software"
)

// InferenceFixture names a built-in demo model (no weights file on disk).
type InferenceFixture string

const (
	InferenceFixtureSplitMLP InferenceFixture = "splitmlp"
)

// DefaultPipelineNodeCount is how many mesh nodes layer shards target in v0.1.
const DefaultPipelineNodeCount = 2

// InferenceModelSpec describes one inference model the daemon can run.
type InferenceModelSpec struct {
	ID         string
	Backend    InferenceBackend
	LayerCount uint32
	ModelPath  string           // local weights path; env vars win in v0.1 subprocess path
	Fixture    InferenceFixture // non-empty for built-in demos
}

// SplitMLPDemoModel is the split-MLP pipeline FIXTURE, exercised by
// `cerberus pipeline-run` and the two-node e2e harnesses.
//
// It is deliberately absent from BuiltinInferenceModels(): a fixture in a test is
// honest, a fixture on /v1/models pretending to be a chat model is not. Callers
// that want it must name it explicitly.
var SplitMLPDemoModel = InferenceModelSpec{
	ID:         "split-mlp-demo",
	Backend:    InferenceBackendCPUSoftware,
	LayerCount: splitMLPLayers,
	Fixture:    InferenceFixtureSplitMLP,
}

// BuiltinInferenceModels returns the models advertised on the gateway's /v1/models.
//
// It is EMPTY, and that is correct. Every entry it used to hold was a mock:
// `llamacpp-mock` ran a fake transform, `tinyllama-1b` ran a fake transform and
// prompted llama-cli with the auth subject, `llama-3.2-1b` required an MLX sidecar
// that rejected any model above 4 layers. Real chat models are registered by
// daemon/llama once a llama-server pack is present — see llama.NewService.
//
// Do not re-add the split-MLP fixture here to make the list look populated.
func BuiltinInferenceModels() []InferenceModelSpec {
	return nil
}

// PipelineFixtureModels returns the fixture registry for the pipeline harnesses
// (`cerberus pipeline-run`, test/pipeline_e2e). These are NOT chat models and are
// not advertised on /v1/models.
func PipelineFixtureModels() []InferenceModelSpec {
	return []InferenceModelSpec{SplitMLPDemoModel}
}

// InferenceResult is the outcome of one inference run.
type InferenceResult struct {
	OK      bool
	Content string // human-readable assistant text
	Output  []byte // raw activation / token bytes from the pipeline
	Backend string
	Node    string // node that ran the final stage
	Stages  []PipelineStageResult
	Error   string
}

// PipelineStageHook is invoked after each successful pipeline stage with the
// activation bytes produced by that stage.
type PipelineStageHook func(stage PipelineStageResult, activation []byte)

// InferenceService runs pipeline-backed inference models.
type InferenceService struct {
	Runner *PipelineRunner
	Models map[string]InferenceModelSpec
}

// NewInferenceService builds a service over an already-composed PipelineRunner.
func NewInferenceService(runner *PipelineRunner, models []InferenceModelSpec) *InferenceService {
	reg := map[string]InferenceModelSpec{}
	for _, m := range models {
		if strings.TrimSpace(m.ID) != "" {
			reg[m.ID] = m
		}
	}
	return &InferenceService{Runner: runner, Models: reg}
}

// Lookup returns a registered model spec.
func (s *InferenceService) Lookup(modelID string) (InferenceModelSpec, bool) {
	m, ok := s.Models[modelID]
	return m, ok
}

// Run executes inference for subject/modelID. input is optional activation bytes
// for the split-MLP fixture; nil uses the default demo vector.
func (s *InferenceService) Run(ctx context.Context, subject, modelID string, input []byte) (InferenceResult, error) {
	return s.RunWithBackend(ctx, subject, modelID, "", input)
}

// RunWithBackend is like Run but optionally overrides the pipeline backend
// (e.g. CLI --backend llamacpp on a registered model).
func (s *InferenceService) RunWithBackend(ctx context.Context, subject, modelID, backendOverride string, input []byte) (InferenceResult, error) {
	spec, ok := s.Models[modelID]
	if !ok {
		return InferenceResult{}, fmt.Errorf("inference: unknown model %q", modelID)
	}
	return s.run(ctx, subject, spec, backendOverride, input, nil)
}

// RunStream executes inference and calls emit with one token string after each
// pipeline stage completes, then a final summary token with the formatted output.
func (s *InferenceService) RunStream(ctx context.Context, subject, modelID string, input []byte, emit func(token string) error) (InferenceResult, error) {
	spec, ok := s.Models[modelID]
	if !ok {
		return InferenceResult{}, fmt.Errorf("inference: unknown model %q", modelID)
	}
	var hook PipelineStageHook
	if emit != nil {
		hook = func(stage PipelineStageResult, _ []byte) {
			_ = emit(formatStageToken(stage))
		}
	}
	res, err := s.run(ctx, subject, spec, "", input, hook)
	if err != nil {
		return res, err
	}
	if emit != nil && res.OK && res.Content != "" {
		if e := emit(res.Content); e != nil {
			return res, e
		}
	}
	return res, nil
}

func (s *InferenceService) run(ctx context.Context, subject string, spec InferenceModelSpec, backendOverride string, input []byte, hook PipelineStageHook) (InferenceResult, error) {
	if s == nil || s.Runner == nil {
		return InferenceResult{}, fmt.Errorf("inference: pipeline runner not available")
	}
	shards, err := shardsForSpec(spec)
	if err != nil {
		return InferenceResult{}, err
	}
	taskID := []byte(subject + ":inference:" + spec.ID)
	if err := setRunnerBackend(s.Runner, spec, backendOverride); err != nil {
		return InferenceResult{}, err
	}
	pipe, err := s.Runner.RunPipelineWithHook(ctx, taskID, shards, input, hook)
	res := InferenceResult{
		OK:      pipe.OK,
		Output:  pipe.Output,
		Backend: pipe.Backend,
		Stages:  pipe.Stages,
		Error:   pipe.Error,
	}
	if len(pipe.Stages) > 0 {
		res.Node = FormatPeerID(pipe.Stages[len(pipe.Stages)-1].Node)
	}
	if err != nil {
		if res.Error == "" {
			res.Error = err.Error()
		}
		return res, err
	}
	if !pipe.OK {
		if res.Error == "" {
			res.Error = "pipeline failed"
		}
		return res, fmt.Errorf("%s", res.Error)
	}
	content, ferr := formatFixtureOutput(spec, pipe.Output)
	if ferr != nil {
		return res, ferr
	}
	res.Content = content
	return res, nil
}

func setRunnerBackend(runner *PipelineRunner, spec InferenceModelSpec, override string) error {
	if override != "" {
		be, err := inference.ParseBackend(override)
		if err != nil {
			return err
		}
		runner.Backend = be
		return nil
	}
	// Only the split-MLP fixture backend exists on this path.
	runner.Backend = inference.BackendCPUSoftware
	return nil
}

// ShardsForLayerCount splits layerCount transformer layers evenly across shardCount
// pipeline shards (inclusive LayerLo/LayerHi ranges).
//
// NOTE: this is Phase-2 substrate, not the real-LLM path. Real llama.cpp inference
// (daemon/llama) does NOT use it: llama.cpp owns its own layer split, distributing
// weights across local+remote devices in proportion to measured memory (overridable
// with --tensor-split). Do not wire this into that path.
func ShardsForLayerCount(layerCount uint32, shardCount int) ([]contract.Shard, error) {
	if layerCount == 0 {
		return nil, fmt.Errorf("inference: layer count must be > 0")
	}
	if shardCount <= 0 {
		return nil, fmt.Errorf("inference: shard count must be > 0")
	}
	base := int(layerCount) / shardCount
	rem := int(layerCount) % shardCount
	shards := make([]contract.Shard, 0, shardCount)
	var lo uint32
	for i := 0; i < shardCount; i++ {
		n := base
		if i < rem {
			n++
		}
		if n == 0 {
			return nil, fmt.Errorf("inference: cannot split %d layers across %d shards", layerCount, shardCount)
		}
		hi := lo + uint32(n) - 1
		shards = append(shards, contract.Shard{
			Kind:    contract.ShardPipeline,
			LayerLo: lo,
			LayerHi: hi,
		})
		lo = hi + 1
	}
	return shards, nil
}

func shardsForSpec(spec InferenceModelSpec) ([]contract.Shard, error) {
	switch spec.Fixture {
	case InferenceFixtureSplitMLP:
		return DefaultSplitMLPShards(), nil
	case "":
		if spec.LayerCount == 0 {
			return nil, fmt.Errorf("inference: model %q has no layer count", spec.ID)
		}
		return ShardsForLayerCount(spec.LayerCount, DefaultPipelineNodeCount)
	default:
		return nil, fmt.Errorf("inference: unsupported fixture %q", spec.Fixture)
	}
}

// formatFixtureOutput renders the split-MLP fixture's output activation.
//
// It takes NO prompt and NO subject, by design. The function this replaced took a
// `prompt string` parameter that its only caller filled with the authorization
// SUBJECT, and then passed that subject to a text-completion call — i.e. the model
// was literally prompted with the auth principal. A subject is an authz identity
// and must never reach a model. There is no prompt on this path at all: the fixture
// consumes an activation vector, not text.
func formatFixtureOutput(spec InferenceModelSpec, output []byte) (string, error) {
	vec, err := DecodeActivation(output)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("[split-mlp fixture %s] activation: [%.4f, %.4f, %.4f, %.4f]",
		spec.Backend, vec[0], vec[1], vec[2], vec[3]), nil
}

func formatStageToken(stage PipelineStageResult) string {
	node := FormatPeerID(stage.Node)
	if len(node) > 8 {
		node = node[:8]
	}
	remote := ""
	if stage.Remote {
		remote = " remote"
	}
	return fmt.Sprintf("[stage L%d-%d @ %s%s ok]\n", stage.Shard.LayerLo, stage.Shard.LayerHi, node, remote)
}
