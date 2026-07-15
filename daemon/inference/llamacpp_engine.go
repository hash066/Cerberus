package inference

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

const llamacppMockBackend = "llamacpp-mock"
const llamacppRealBackend = "llamacpp"

// llamacppForwardActivation runs pipeline shard layers [layerLo, layerHi].
func llamacppForwardActivation(input Activation, layerLo, layerHi uint32) (Activation, string, error) {
	if layerLo > layerHi {
		return Activation{}, "", fmt.Errorf("llamacpp: invalid layer range [%d,%d]", layerLo, layerHi)
	}
	if mockForced() || !llamacppSupported() {
		return llamacppMockForward(input, layerLo, layerHi)
	}
	if !llamaModelConfigured() || !LlamacppHelperReady() {
		return llamacppMockForward(input, layerLo, layerHi)
	}
	return llamacppRealForwardActivation(input, layerLo, layerHi)
}

func llamacppMockForward(input Activation, layerLo, layerHi uint32) (Activation, string, error) {
	vec, err := input.AsSplitMLP()
	if err != nil {
		return Activation{}, "", err
	}
	out, err := mockForwardRange(vec, layerLo, layerHi)
	if err != nil {
		return Activation{}, "", err
	}
	return ActivationFromSplitMLP(out), llamacppMockBackend, nil
}

func llamacppRealForwardActivation(input Activation, layerLo, layerHi uint32) (Activation, string, error) {
	h, err := sharedLlamacppHelper()
	if err != nil {
		return Activation{}, "", err
	}
	model, err := resolveModelPath()
	if err != nil {
		return Activation{}, "", err
	}
	req := llamacppRequest{
		Op:              "forward_layers",
		LayerLo:         layerLo,
		LayerHi:         layerHi,
		Model:           model,
		ActivationBytes: input.EncodeActivationBase64(),
		Shape:           append([]uint32(nil), input.Shape...),
		DType:           string(input.DType),
	}
	if layerLo == 0 && input.IsSplitMLPFixed() {
		req.Prompt = llamaDefaultPrompt()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	resp, err := h.call(ctx, req)
	if err != nil {
		return Activation{}, "", err
	}
	if !resp.OK {
		return Activation{}, "", fmt.Errorf("llamacpp: forward_layers: %s", resp.Error)
	}
	if resp.ActivationBytes == "" {
		return Activation{}, "", fmt.Errorf("llamacpp: forward_layers: missing activation_bytes")
	}
	dtype := TensorDType(resp.DType)
	if dtype == "" {
		dtype = TensorDTypeF32
	}
	out, err := DecodeActivationBase64(resp.ActivationBytes, resp.Shape, dtype)
	if err != nil {
		return Activation{}, "", err
	}
	return out, llamacppRealBackend, nil
}

func llamaModelConfigured() bool {
	_, err := resolveModelPath()
	return err == nil
}

func llamaDefaultPrompt() string {
	if p := strings.TrimSpace(os.Getenv("CERBERUS_LLAMA_PROMPT")); p != "" {
		return p
	}
	return "Hello"
}

// LlamacppAvailable reports whether this platform builds the llamacpp subprocess path.
func LlamacppAvailable() bool {
	return llamacppSupported()
}

// LlamacppCLIReady reports whether a llama.cpp CLI binary was found on PATH or via
// CERBERUS_LLAMA_CLI.
func LlamacppCLIReady() bool {
	_, err := resolveCLIPath()
	return err == nil
}
