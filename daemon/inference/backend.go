// Package inference selects pipeline inference backends and exposes a layer-range
// forward interface matching PipelineRunner shard placement (LayerLo/LayerHi).
//
// Backends:
//   - cpu-software: deterministic 4-layer SplitMLP fixture (default)
//   - llamacpp: llama.cpp helper when model+sidecar present; mock shard
//     forward otherwise. Real token generation via Complete().
//   - mlx: Python MLX sidecar (macOS/Apple Silicon only); shard forward runs
//     as real mlx arrays when the sidecar is ready, pure-Go mock otherwise.
//     Real token generation via MLXGenerate() / MLXForwardPass().
package inference

import (
	"fmt"
	"strings"
)

// Backend names the pipeline inference engine.
type Backend string

const (
	BackendCPUSoftware Backend = "cpu-software"
	BackendLlamacpp    Backend = "llamacpp"
	BackendMLX         Backend = "mlx"
)

// ParseBackend maps a CLI/env string to a Backend.
func ParseBackend(name string) (Backend, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", string(BackendCPUSoftware), "splitmlp", "cpu":
		return BackendCPUSoftware, nil
	case string(BackendLlamacpp), "llama", "llama.cpp", "ggml":
		return BackendLlamacpp, nil
	case string(BackendMLX):
		return BackendMLX, nil
	default:
		return "", fmt.Errorf("inference: unknown backend %q (want cpu-software, llamacpp, or mlx)", name)
	}
}

// ForwardRange runs layers [layerLo, layerHi] inclusive on input activation
// bytes and returns output bytes plus the backend name that actually ran.
// Legacy split-MLP shards pass exactly ActivationBytes with no shape metadata.
func ForwardRange(backend Backend, input []byte, layerLo, layerHi uint32) ([]byte, string, error) {
	act, err := ActivationFromBytes(input, nil, TensorDTypeF32)
	if err != nil {
		return nil, "", err
	}
	out, reported, err := ForwardActivation(backend, act, layerLo, layerHi)
	if err != nil {
		return nil, "", err
	}
	return out.PayloadOnly(), reported, nil
}

// ForwardActivation runs layers [layerLo, layerHi] on a structured activation
// (fixed split-MLP fixture or variable token/hidden tensors for real models).
func ForwardActivation(backend Backend, input Activation, layerLo, layerHi uint32) (Activation, string, error) {
	if err := input.normalize(); err != nil {
		return Activation{}, "", err
	}
	switch backend {
	case BackendLlamacpp:
		return llamacppForwardActivation(input, layerLo, layerHi)
	case BackendMLX:
		return mlxForwardActivation(input, layerLo, layerHi)
	default:
		vec, err := input.AsSplitMLP()
		if err != nil {
			return Activation{}, "", err
		}
		out, err := splitMLPForwardRange(vec, layerLo, layerHi)
		if err != nil {
			return Activation{}, "", err
		}
		return ActivationFromSplitMLP(out), string(BackendCPUSoftware), nil
	}
}

// ComponentTag returns the mesh compute component identifier for a backend.
func ComponentTag(backend Backend) []byte {
	switch backend {
	case BackendLlamacpp:
		return []byte("llamacpp/subprocess")
	case BackendMLX:
		return []byte("mlx/python-sidecar")
	default:
		return []byte("splitmlp/go-fixture")
	}
}

// IsPipelineComponent reports whether a compute task component targets pipeline inference.
func IsPipelineComponent(component []byte) bool {
	s := string(component)
	return s == "splitmlp/go-fixture" || s == "llamacpp/subprocess" || s == "mlx/python-sidecar"
}

// BackendFromComponent maps a pipeline component tag to a Backend.
func BackendFromComponent(component []byte) Backend {
	switch string(component) {
	case "llamacpp/subprocess":
		return BackendLlamacpp
	case "mlx/python-sidecar":
		return BackendMLX
	default:
		return BackendCPUSoftware
	}
}
