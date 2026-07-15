// inference.go is the shared entry point for pipeline-backed inference demos.
// Gateway chat completions and `cerberus pipeline-run` both call InferenceService
// so they exercise the same PipelineRunner path. v0.1 uses the honest split-MLP
// cpu-software fixture; MLX / llama.cpp backends plug in via InferenceModelSpec.
//
// Model support matrix (v0.1.1, honest):
//
//	| Model ID        | Backend    | Platform        | Weights / deps              | Pipeline shards |
//	|-----------------|------------|-----------------|-----------------------------|-----------------|
//	| split-mlp-demo  | cpu-soft   | all             | built-in fixture            | 2 (4 layers)    |
//	| llamacpp-mock   | llama.cpp  | all             | mock shard forward (CI)     | 2 (4 layers)    |
//	| tinyllama-1b    | llama.cpp  | Win/Linux       | CERBERUS_LLAMA_MODEL + CLI  | 2 (22 layers)   |
//	| llama-3.2-1b    | mlx        | macOS AS + MLX  | CERBERUS_MLX_MODEL + mlx-lm | 2 (16 layers)   |
//
// Without weights the LLM entries still run: shard forward uses deterministic
// mock transforms; Complete()/MLXGenerate() produce real tokens only when deps
// are present. CI always uses llamacpp-mock or split-mlp-demo.
package system

import (
	"context"
	"fmt"
	"strings"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/inference"
)

// InferenceBackend names the compute backend for an inference model.
type InferenceBackend string

const (
	InferenceBackendCPUSoftware InferenceBackend = "cpu-software"
	InferenceBackendMLX         InferenceBackend = "mlx"
	InferenceBackendLlamaCpp    InferenceBackend = "llama.cpp"
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

// SplitMLPDemoModel is the canonical split-MLP pipeline fixture advertised on
// /v1/models and exercised by gateway streaming + `cerberus pipeline-run`.
var SplitMLPDemoModel = InferenceModelSpec{
	ID:         "split-mlp-demo",
	Backend:    InferenceBackendCPUSoftware,
	LayerCount: splitMLPLayers,
	Fixture:    InferenceFixtureSplitMLP,
}

// LlamaCppMockModel exercises the llamacpp pipeline backend with mock shard
// forward (no GGUF weights). Set CERBERUS_LLAMA_MODEL + llama-cli for real tokens
// via inference.Complete in formatInferenceOutput.
var LlamaCppMockModel = InferenceModelSpec{
	ID:         "llamacpp-mock",
	Backend:    InferenceBackendLlamaCpp,
	LayerCount: 4,
	Fixture:    InferenceFixtureSplitMLP,
}

// TinyLlamaModel is TinyLlama-1.1B (22 transformer layers). Requires Windows or
// Linux with llama.cpp CLI + GGUF at CERBERUS_LLAMA_MODEL for real tokens; shard
// forward stays mock until llama.cpp layer-range FFI lands.
var TinyLlamaModel = InferenceModelSpec{
	ID:         "tinyllama-1b",
	Backend:    InferenceBackendLlamaCpp,
	LayerCount: 22,
}

// Llama32Model is Llama 3.2 1B Instruct (16 layers). Requires macOS Apple
// Silicon with mlx-lm and CERBERUS_MLX_MODEL for real shard forward / tokens.
var Llama32Model = InferenceModelSpec{
	ID:         "llama-3.2-1b",
	Backend:    InferenceBackendMLX,
	LayerCount: 16,
}

// BuiltinInferenceModels returns the v0.1.1 demo registry.
func BuiltinInferenceModels() []InferenceModelSpec {
	return []InferenceModelSpec{
		SplitMLPDemoModel,
		LlamaCppMockModel,
		TinyLlamaModel,
		Llama32Model,
	}
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
	content, ferr := formatInferenceOutput(ctx, subject, spec, pipe.Output)
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
	switch spec.Backend {
	case InferenceBackendLlamaCpp:
		runner.Backend = inference.BackendLlamacpp
	case InferenceBackendMLX:
		runner.Backend = inference.BackendMLX
	default:
		runner.Backend = inference.BackendCPUSoftware
	}
	return nil
}

// ShardsForLayerCount splits layerCount transformer layers evenly across shardCount
// pipeline shards (inclusive LayerLo/LayerHi ranges). Used by real LLM models.
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

func formatInferenceOutput(ctx context.Context, prompt string, spec InferenceModelSpec, output []byte) (string, error) {
	switch spec.Backend {
	case InferenceBackendLlamaCpp:
		if inference.LlamacppCLIReady() {
			if _, err := inference.ModelPathForTest(); err == nil {
				text, backend, err := inference.Complete(ctx, prompt, 16)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("[%s] %s", backend, text), nil
			}
		}
		vec, err := DecodeActivation(output)
		if err != nil {
			return "", err
		}
		tag := spec.ID
		if tag == "" {
			tag = "llamacpp-mock"
		}
		return fmt.Sprintf("[%s] activation: [%.4f, %.4f, %.4f, %.4f]",
			tag, vec[0], vec[1], vec[2], vec[3]), nil
	case InferenceBackendMLX:
		if inference.MLXSidecarReady() && inference.MLXAvailable() {
			text, backend, err := inference.MLXGenerate(ctx, prompt, 16)
			if err == nil {
				return fmt.Sprintf("[%s] %s", backend, text), nil
			}
		}
		vec, err := DecodeActivation(output)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("[%s mlx-mock] activation: [%.4f, %.4f, %.4f, %.4f]",
			spec.ID, vec[0], vec[1], vec[2], vec[3]), nil
	default:
		vec, err := DecodeActivation(output)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("[split-mlp %s] activation: [%.4f, %.4f, %.4f, %.4f]",
			spec.Backend, vec[0], vec[1], vec[2], vec[3]), nil
	}
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
