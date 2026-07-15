package inference

import (
	"fmt"
	"os"
	"strings"
)

func mockForced() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("CERBERUS_LLAMACPP_MOCK")))
	return v == "1" || v == "true" || v == "yes"
}

// mockForwardRange applies deterministic mock transformer layers for pipeline
// shard tests when no GGUF model or llama.cpp binary is present.
func mockForwardRange(input [ActivationDim]float32, layerLo, layerHi uint32) ([ActivationDim]float32, error) {
	if layerLo > layerHi {
		return input, fmt.Errorf("llamacpp-mock: invalid layer range [%d,%d]", layerLo, layerHi)
	}
	out := input
	for layer := layerLo; layer <= layerHi; layer++ {
		out = applyMockLayer(out, int(layer), int(layerHi))
	}
	return out, nil
}

func applyMockLayer(in [ActivationDim]float32, layer, layerHi int) [ActivationDim]float32 {
	var out [ActivationDim]float32
	for i := 0; i < ActivationDim; i++ {
		sum := mockLayerBias(layer, i)
		for j := 0; j < ActivationDim; j++ {
			sum += mockLayerWeight(layer, i, j) * in[j]
		}
		if layer < layerHi {
			sum = relu(sum)
		}
		out[i] = sum
	}
	return out
}

func mockLayerWeight(layer, row, col int) float32 {
	// Distinct from splitmlp weights so tests can tell backends apart.
	return float32(layer+2)*0.07 + float32(col)*0.013 + float32(row)*0.002
}

func mockLayerBias(layer, row int) float32 {
	return float32(layer)*0.03 + float32(row)*0.01
}
