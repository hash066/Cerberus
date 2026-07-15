// splitmlp.go is a minimal, honest 4-layer MLP fixture for pipeline demos.
//
// It runs on the CPU in pure Go (no MLX/tinygrad, no fake GPU). Weights are
// fixed and deterministic so two-node pipeline tests can assert an exact final
// activation. Each pipeline shard executes layers [LayerLo, LayerHi] inclusive.
package system

import (
	"encoding/binary"
	"fmt"
	"math"

	contract "github.com/hash066/cerberus/contract/go"
)

const (
	splitMLPLayers = 4
	splitMLPDim    = 4
)

// SplitMLPDefaultInput is the canonical demo input vector [1, 0, 0, 0].
var SplitMLPDefaultInput = [splitMLPDim]float32{1, 0, 0, 0}

// SplitMLP runs a tiny fully-connected network used by PipelineRunner demos.
type SplitMLP struct{}

// ForwardRange applies layers [layerLo, layerHi] inclusive to input and returns
// the activation after the last layer in the range.
func (SplitMLP) ForwardRange(input [splitMLPDim]float32, layerLo, layerHi uint32) ([splitMLPDim]float32, error) {
	if layerLo > layerHi || layerHi >= splitMLPLayers {
		return input, fmt.Errorf("splitmlp: invalid layer range [%d,%d]", layerLo, layerHi)
	}
	out := input
	for layer := layerLo; layer <= layerHi; layer++ {
		out = applyLayer(out, int(layer))
	}
	return out, nil
}

// ForwardFull runs all four layers.
func (m SplitMLP) ForwardFull(input [splitMLPDim]float32) ([splitMLPDim]float32, error) {
	return m.ForwardRange(input, 0, splitMLPLayers-1)
}

// ExpectedOutput returns the reference result for SplitMLPDefaultInput through
// all four layers — used by tests to assert end-to-end correctness.
func (SplitMLP) ExpectedOutput() [splitMLPDim]float32 {
	out, err := (SplitMLP{}).ForwardFull(SplitMLPDefaultInput)
	if err != nil {
		panic(err)
	}
	return out
}

func applyLayer(in [splitMLPDim]float32, layer int) [splitMLPDim]float32 {
	var out [splitMLPDim]float32
	for i := 0; i < splitMLPDim; i++ {
		sum := layerBias(layer, i)
		for j := 0; j < splitMLPDim; j++ {
			sum += layerWeight(layer, i, j) * in[j]
		}
		if layer < splitMLPLayers-1 {
			sum = relu(sum)
		}
		out[i] = sum
	}
	return out
}

func layerWeight(layer, row, col int) float32 {
	return float32(layer+1)*0.1 + float32(col)*0.01 + float32(row)*0.001
}

func layerBias(layer, row int) float32 {
	return float32(layer) * 0.05
}

func relu(x float32) float32 {
	if x < 0 {
		return 0
	}
	return x
}

// splitMLPActivationShape is the canonical 1-D shape for the v0.1 split-MLP fixture.
var splitMLPActivationShape = []uint32{splitMLPDim}

// WrapSplitMLPActivation tags raw split-MLP f32 bytes as a contract ActivationFrame.
func WrapSplitMLPActivation(raw []byte) (contract.ActivationFrame, error) {
	return contract.ActivationFrameFromF32Bytes(splitMLPActivationShape, raw)
}
func EncodeActivation(v [splitMLPDim]float32) []byte {
	b := make([]byte, splitMLPDim*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

// DecodeActivation parses a little-endian f32 activation vector.
func DecodeActivation(b []byte) ([splitMLPDim]float32, error) {
	var out [splitMLPDim]float32
	if len(b) != splitMLPDim*4 {
		return out, fmt.Errorf("splitmlp: activation must be %d bytes, got %d", splitMLPDim*4, len(b))
	}
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out, nil
}
