// mlx_engine.go selects between the real MLX sidecar and the honest mock
// path for the "mlx" pipeline backend, and exposes the single-node real-model
// surfaces (MLXForwardPass, MLXGenerate).
//
// MATURITY HONESTY:
//   - Real MLX compute needs macOS on Apple Silicon + `pip install mlx mlx-lm`.
//     With CERBERUS_MLX_MODEL set the pipeline shard forward runs real model
//     layers via op "forward_layers" (backend "mlx"). Without a model path the
//     sidecar runs the split-MLP fixture as real mlx arrays (still "mlx").
//   - Anywhere else, or with CERBERUS_MLX_MOCK=1, shard forward falls back to
//     the same deterministic split-MLP weights in pure Go and reports "mlx-mock".
package inference

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
)

const (
	mlxMockBackend = "mlx-mock"
	mlxRealBackend = "mlx"
)

// mlxShared is the lazily-started, process-wide sidecar. One probe decides
// readiness for the daemon's lifetime; a dead sidecar is not restarted in
// v0.1 (restart-on-failure is future work, documented in mlx/README.md).
var (
	mlxMu       sync.Mutex
	mlxSidecar  *MLXSidecar
	mlxProbed   bool
	mlxProbeErr error
	mlxHasMLX   bool
	mlxHasMLXLM bool
)

// mlxForwardActivation runs pipeline shard layers [layerLo, layerHi].
func mlxForwardActivation(input Activation, layerLo, layerHi uint32) (Activation, string, error) {
	if mlxMockForced() || !MLXSidecarReady() {
		vec, err := input.AsSplitMLP()
		if err != nil {
			return Activation{}, "", err
		}
		if layerLo > layerHi {
			return Activation{}, "", fmt.Errorf("mlx: invalid layer range [%d,%d]", layerLo, layerHi)
		}
		if layerHi >= splitMLPLayers {
			out, err := mockForwardRange(vec, layerLo, layerHi)
			if err != nil {
				return Activation{}, "", err
			}
			return ActivationFromSplitMLP(out), mlxMockBackend, nil
		}
		out, err := splitMLPForwardRange(vec, layerLo, layerHi)
		if err != nil {
			return Activation{}, "", err
		}
		return ActivationFromSplitMLP(out), mlxMockBackend, nil
	}

	if mlxModelConfigured() {
		return mlxForwardLayersReal(input, layerLo, layerHi)
	}

	// No model path: run the split-MLP fixture as real mlx arrays.
	vec, err := input.AsSplitMLP()
	if err != nil {
		return Activation{}, "", fmt.Errorf("mlx: split-MLP fixture required without CERBERUS_MLX_MODEL: %w", err)
	}
	if layerLo > layerHi || layerHi >= splitMLPLayers {
		return Activation{}, "", fmt.Errorf("mlx: invalid layer range [%d,%d]", layerLo, layerHi)
	}
	sc, err := sharedMLXSidecar()
	if err != nil {
		return Activation{}, "", err
	}
	resp, err := sc.call(context.Background(), mlxRequest{
		Op:         "forward_splitmlp",
		LayerLo:    layerLo,
		LayerHi:    layerHi,
		Activation: vec[:],
	})
	if err != nil {
		return Activation{}, "", err
	}
	if !resp.OK {
		return Activation{}, "", fmt.Errorf("mlx: sidecar forward: %s", resp.Error)
	}
	if len(resp.Activation) != ActivationDim {
		return Activation{}, "", fmt.Errorf("mlx: sidecar returned %d floats, want %d", len(resp.Activation), ActivationDim)
	}
	var out [ActivationDim]float32
	copy(out[:], resp.Activation)
	return ActivationFromSplitMLP(out), mlxRealBackend, nil
}

func mlxForwardLayersReal(input Activation, layerLo, layerHi uint32) (Activation, string, error) {
	sc, err := readySidecarForModelOps()
	if err != nil {
		return Activation{}, "", err
	}
	req := mlxRequest{
		Op:              "forward_layers",
		LayerLo:         layerLo,
		LayerHi:         layerHi,
		Model:           mlxModelPath(),
		ActivationBytes: input.EncodeActivationBase64(),
		Shape:           append([]uint32(nil), input.Shape...),
		DType:           string(input.DType),
	}
	if layerLo == 0 && input.IsSplitMLPFixed() {
		req.Prompt = mlxDefaultPrompt()
	}
	resp, err := sc.call(context.Background(), req)
	if err != nil {
		return Activation{}, "", err
	}
	if !resp.OK {
		return Activation{}, "", fmt.Errorf("mlx: forward_layers: %s", resp.Error)
	}
	if resp.ActivationBytes == "" {
		return Activation{}, "", fmt.Errorf("mlx: forward_layers: missing activation_bytes")
	}
	dtype := TensorDType(resp.DType)
	if dtype == "" {
		dtype = TensorDTypeF32
	}
	out, err := DecodeActivationBase64(resp.ActivationBytes, resp.Shape, dtype)
	if err != nil {
		return Activation{}, "", err
	}
	return out, mlxRealBackend, nil
}

// MLXForwardResult is one real forward pass over the full layer range:
// last-position top-k next-token logits from the configured model.
type MLXForwardResult struct {
	TopTokens  []MLXTopToken
	NLayers    int
	HiddenSize int
	Backend    string
}

// MLXForwardPass runs ONE real forward pass (all layers, no KV cache) of the
// configured model on prompt and returns the top next-token logits. Requires
// a ready sidecar with mlx-lm; the first call downloads the model weights.
func MLXForwardPass(ctx context.Context, prompt string) (MLXForwardResult, error) {
	sc, err := readySidecarForModelOps()
	if err != nil {
		return MLXForwardResult{}, err
	}
	resp, err := sc.call(ctx, mlxRequest{Op: "forward", Prompt: prompt, Model: mlxModelPath()})
	if err != nil {
		return MLXForwardResult{}, err
	}
	if !resp.OK {
		return MLXForwardResult{}, fmt.Errorf("mlx: forward: %s", resp.Error)
	}
	return MLXForwardResult{
		TopTokens:  resp.TopTokens,
		NLayers:    resp.NLayers,
		HiddenSize: resp.HiddenSize,
		Backend:    mlxRealBackend,
	}, nil
}

// MLXGenerate runs single-node text generation via mlx_lm (the mlx analogue
// of llamacpp's Complete). Returns generated text and the backend that ran.
func MLXGenerate(ctx context.Context, prompt string, maxTokens int) (string, string, error) {
	sc, err := readySidecarForModelOps()
	if err != nil {
		return "", "", err
	}
	if maxTokens <= 0 {
		maxTokens = 8
	}
	resp, err := sc.call(ctx, mlxRequest{
		Op:        "generate",
		Prompt:    prompt,
		MaxTokens: maxTokens,
		Model:     mlxModelPath(),
	})
	if err != nil {
		return "", "", err
	}
	if !resp.OK {
		return "", "", fmt.Errorf("mlx: generate: %s", resp.Error)
	}
	return resp.Text, mlxRealBackend, nil
}

// MLXAvailable reports whether this build targets a platform that can run mlx
// at all (darwin). It says nothing about python/mlx actually being installed.
func MLXAvailable() bool {
	return mlxSupportedPlatform
}

// MLXSidecarReady reports whether the sidecar started AND mlx is importable
// inside it. Probed once per process; false on non-darwin without spawning.
func MLXSidecarReady() bool {
	if !mlxSupportedPlatform || mlxMockForced() {
		return false
	}
	mlxMu.Lock()
	defer mlxMu.Unlock()
	probeMLXLocked()
	return mlxProbeErr == nil && mlxHasMLX
}

// MLXReportedBackend is the honest PipelineResult backend string for the mlx
// backend on this node: "mlx" only when the sidecar genuinely computes.
func MLXReportedBackend() string {
	if mlxMockForced() || !mlxSupportedPlatform {
		return mlxMockBackend
	}
	if !MLXSidecarReady() {
		return mlxMockBackend
	}
	if mlxModelConfigured() {
		mlxMu.Lock()
		hasLM := mlxHasMLXLM
		mlxMu.Unlock()
		if !hasLM {
			return mlxMockBackend
		}
	}
	return mlxRealBackend
}

func sharedMLXSidecar() (*MLXSidecar, error) {
	mlxMu.Lock()
	defer mlxMu.Unlock()
	probeMLXLocked()
	if mlxProbeErr != nil {
		return nil, mlxProbeErr
	}
	return mlxSidecar, nil
}

// readySidecarForModelOps gates the real-model surfaces: platform, sidecar,
// mlx AND mlx-lm must all be present, otherwise a clear error explains what
// is missing (never a fake result).
func readySidecarForModelOps() (*MLXSidecar, error) {
	if !mlxSupportedPlatform {
		return nil, fmt.Errorf("mlx: unsupported platform (mlx requires macOS on Apple Silicon)")
	}
	if mlxMockForced() {
		return nil, fmt.Errorf("mlx: mock mode enabled (CERBERUS_MLX_MOCK=1)")
	}
	if !mlxModelConfigured() {
		return nil, fmt.Errorf("mlx: CERBERUS_MLX_MODEL not set")
	}
	sc, err := sharedMLXSidecar()
	if err != nil {
		return nil, err
	}
	mlxMu.Lock()
	hasMLX, hasMLXLM := mlxHasMLX, mlxHasMLXLM
	mlxMu.Unlock()
	if !hasMLX {
		return nil, fmt.Errorf("mlx: mlx not importable in sidecar (pip install mlx)")
	}
	if !hasMLXLM {
		return nil, fmt.Errorf("mlx: mlx-lm not importable in sidecar (pip install mlx-lm)")
	}
	return sc, nil
}

// probeMLXLocked starts the sidecar and reads op "info" exactly once.
// Callers hold mlxMu.
func probeMLXLocked() {
	if mlxProbed {
		return
	}
	mlxProbed = true
	sc, err := startMLXSidecar()
	if err != nil {
		mlxProbeErr = err
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), mlxProbeTimeout)
	defer cancel()
	info, err := sc.call(ctx, mlxRequest{Op: "info"})
	if err != nil {
		sc.Close()
		mlxProbeErr = err
		return
	}
	mlxSidecar = sc
	mlxHasMLX = info.MLX
	mlxHasMLXLM = info.MLXLM
}

func mlxMockForced() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("CERBERUS_MLX_MOCK")))
	return v == "1" || v == "true" || v == "yes"
}

func mlxModelConfigured() bool {
	return mlxModelPath() != ""
}

func mlxModelPath() string {
	return strings.TrimSpace(os.Getenv("CERBERUS_MLX_MODEL"))
}

func mlxDefaultPrompt() string {
	if p := strings.TrimSpace(os.Getenv("CERBERUS_MLX_PROMPT")); p != "" {
		return p
	}
	return "Hello"
}
