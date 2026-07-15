package inference

import "fmt"

const splitMLPLayers = 4

func splitMLPForwardRange(input [ActivationDim]float32, layerLo, layerHi uint32) ([ActivationDim]float32, error) {
	if layerLo > layerHi || layerHi >= splitMLPLayers {
		return input, fmt.Errorf("splitmlp: invalid layer range [%d,%d]", layerLo, layerHi)
	}
	out := input
	for layer := layerLo; layer <= layerHi; layer++ {
		out = applySplitMLPLayer(out, int(layer))
	}
	return out, nil
}

func splitMLPForwardFull(input [ActivationDim]float32) ([ActivationDim]float32, error) {
	return splitMLPForwardRange(input, 0, splitMLPLayers-1)
}

func splitMLPExpectedOutput() [ActivationDim]float32 {
	out, err := splitMLPForwardFull(DefaultActivation)
	if err != nil {
		panic(err)
	}
	return out
}

func applySplitMLPLayer(in [ActivationDim]float32, layer int) [ActivationDim]float32 {
	var out [ActivationDim]float32
	for i := 0; i < ActivationDim; i++ {
		sum := splitMLPLayerBias(layer, i)
		for j := 0; j < ActivationDim; j++ {
			sum += splitMLPLayerWeight(layer, i, j) * in[j]
		}
		if layer < splitMLPLayers-1 {
			sum = relu(sum)
		}
		out[i] = sum
	}
	return out
}

func splitMLPLayerWeight(layer, row, col int) float32 {
	return float32(layer+1)*0.1 + float32(col)*0.01 + float32(row)*0.001
}

func splitMLPLayerBias(layer, row int) float32 {
	return float32(layer) * 0.05
}

func relu(x float32) float32 {
	if x < 0 {
		return 0
	}
	return x
}
