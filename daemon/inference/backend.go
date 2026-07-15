// Package inference provides the layer-range forward interface used by
// PipelineRunner shard placement (LayerLo/LayerHi).
//
// SCOPE — read this before adding anything here:
//
// This package contains exactly ONE backend: `cpu-software`, a deterministic
// 4-layer / 4-dimension SplitMLP fixture whose weights come from a formula. It is
// a TEST FIXTURE. It is not a language model, it has no tokenizer, no weights on
// disk, no KV-cache and no sampler. Its only job is to prove that Cerberus's own
// machinery — mesh streams, the dataplane, the scheduler, signed capability gates —
// carries an activation across nodes and back. For that job it is honest and
// valuable, and daemon/system/pipeline_integration_test.go depends on it.
//
// It is deliberately NOT advertised on /v1/models. A fixture inside a test is
// honest; a fixture on a public model list is a lie.
//
// REAL large-language-model inference does NOT live here. It lives in daemon/llama,
// which supervises upstream llama.cpp (llama-server + ggml-rpc-server) and tunnels
// the ggml RPC byte stream over a capability-gated mesh session. That code owns the
// tokenizer, the KV-cache, the sampler and the layer split, because llama.cpp
// already implements all four correctly and this repo does not.
//
// Historical note: v0.1 shipped `llamacpp` and `mlx` backends here. Both were mock
// transforms named after real engines — the llama.cpp C++ helper had never been
// compiled and was protocol-broken, and the MLX sidecar rejected any model with
// more than 4 layers. They were deleted rather than repaired. The one genuinely
// correct fragment (a real layer-range forward) is preserved as reference material
// in docs/verticals/11-pipeline-layer-range-reference.md.
package inference

import (
	"fmt"
	"strings"
)

// Backend names the pipeline inference engine. There is exactly one, and it is a
// fixture — see the package doc.
type Backend string

const (
	// BackendCPUSoftware is the deterministic SplitMLP test fixture.
	BackendCPUSoftware Backend = "cpu-software"
)

// ParseBackend maps a CLI/env string to a Backend.
//
// "llamacpp" and "mlx" are rejected with an explicit explanation rather than
// silently falling back to the fixture: a caller asking for llama.cpp must never
// be handed a 4-float toy and told it succeeded.
func ParseBackend(name string) (Backend, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", string(BackendCPUSoftware), "splitmlp", "cpu":
		return BackendCPUSoftware, nil
	case "llamacpp", "llama", "llama.cpp", "ggml", "mlx":
		return "", fmt.Errorf(
			"inference: backend %q is not provided by this package — the %q mock was removed; "+
				"real llama.cpp inference lives in daemon/llama (llama-server + ggml-rpc-server), "+
				"not behind this flag", name, name)
	default:
		return "", fmt.Errorf("inference: unknown backend %q (only %q is available; it is a test fixture)", name, BackendCPUSoftware)
	}
}

// ForwardRange runs layers [layerLo, layerHi] inclusive on input activation bytes
// and returns output bytes plus the backend name that actually ran. Split-MLP
// shards pass exactly ActivationBytes with no shape metadata.
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

// ForwardActivation runs layers [layerLo, layerHi] on a structured activation.
func ForwardActivation(backend Backend, input Activation, layerLo, layerHi uint32) (Activation, string, error) {
	if err := input.normalize(); err != nil {
		return Activation{}, "", err
	}
	if backend != "" && backend != BackendCPUSoftware {
		return Activation{}, "", fmt.Errorf("inference: no such backend %q (only %q is available)", backend, BackendCPUSoftware)
	}
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

// ComponentTag returns the mesh compute component identifier for a backend.
func ComponentTag(_ Backend) []byte {
	return []byte("splitmlp/go-fixture")
}

// IsPipelineComponent reports whether a compute task component targets pipeline
// inference.
func IsPipelineComponent(component []byte) bool {
	return string(component) == "splitmlp/go-fixture"
}

// BackendFromComponent maps a pipeline component tag to a Backend.
func BackendFromComponent(_ []byte) Backend {
	return BackendCPUSoftware
}
