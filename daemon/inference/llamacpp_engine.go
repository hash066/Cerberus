package inference

import (
	"fmt"
	"os"
	"strings"
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
