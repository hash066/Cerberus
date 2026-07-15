package inference

import (
	"encoding/binary"
	"fmt"
	"math"

	contract "github.com/hash066/cerberus/contract/go"
)

// ModelActivationConfig describes hidden-state dimensions for real LLM activations.
type ModelActivationConfig struct {
	BatchSize  uint32 // zero means 1
	SeqLen     uint32
	HiddenSize uint32
}

// Shape returns row-major [batch, seq, hidden].
func (c ModelActivationConfig) Shape() []uint32 {
	batch := c.BatchSize
	if batch == 0 {
		batch = 1
	}
	return []uint32{batch, c.SeqLen, c.HiddenSize}
}

// Volume returns the number of elements implied by the config.
func (c ModelActivationConfig) Volume() (uint64, error) {
	return shapeVolume(c.Shape())
}

// ConfigFromShape infers config from a 3-D [batch, seq, hidden] shape.
func ConfigFromShape(shape []uint32) (ModelActivationConfig, error) {
	if len(shape) != 3 {
		return ModelActivationConfig{}, fmt.Errorf("inference: hidden state shape want [batch,seq,hidden], got %v", shape)
	}
	return ModelActivationConfig{
		BatchSize:  shape[0],
		SeqLen:     shape[1],
		HiddenSize: shape[2],
	}, nil
}

// KVCacheHandle is optional cross-shard KV cache metadata.
// v0.1 stub: the handle is recorded for backends but cache tensors are not
// transferred, persisted, or restored across pipeline stages.
type KVCacheHandle struct {
	Handle string
	Layer  uint32
}

// StubStatus documents the v0.1 honesty boundary for KV cache plumbing.
func (k *KVCacheHandle) StubStatus() string {
	if k == nil || k.Handle == "" {
		return ""
	}
	return "v0.1 stub: KV cache handle recorded; tensor bytes not transferred"
}

// ActivationBundle is the backend-facing view of one inbound activation.
type ActivationBundle struct {
	Frame   contract.ActivationFrame
	Config  ModelActivationConfig
	KVCache *KVCacheHandle
}

// EncodeHiddenStateF32 builds a contract ActivationFrame for an LLM hidden state.
func EncodeHiddenStateF32(cfg ModelActivationConfig, values []float32) (contract.ActivationFrame, error) {
	return contract.ActivationFrameF32(cfg.Shape(), values)
}

// DecodeHiddenStateF32 decodes a hidden-state frame into values and config.
func DecodeHiddenStateF32(frame contract.ActivationFrame) ([]float32, ModelActivationConfig, error) {
	cfg, err := ConfigFromShape(frame.Shape)
	if err != nil {
		return nil, ModelActivationConfig{}, err
	}
	values, err := decodeF32Payload(frame)
	if err != nil {
		return nil, ModelActivationConfig{}, err
	}
	return values, cfg, nil
}

// BundleFromFrame builds a backend-facing ActivationBundle from a wire frame.
func BundleFromFrame(frame contract.ActivationFrame, kv *KVCacheHandle) (ActivationBundle, error) {
	if err := frame.Validate(); err != nil {
		return ActivationBundle{}, err
	}
	var cfg ModelActivationConfig
	if len(frame.Shape) == 3 {
		cfg, _ = ConfigFromShape(frame.Shape)
	} else if len(frame.Shape) == 1 && frame.Shape[0] == ActivationDim {
		cfg = ModelActivationConfig{BatchSize: 1, SeqLen: 1, HiddenSize: ActivationDim}
	}
	return ActivationBundle{Frame: frame, Config: cfg, KVCache: kv}, nil
}

// PayloadBytes returns raw tensor bytes for backend forward paths.
func (b ActivationBundle) PayloadBytes() []byte {
	return append([]byte(nil), b.Frame.Payload...)
}

func decodeF32Payload(frame contract.ActivationFrame) ([]float32, error) {
	if err := frame.Validate(); err != nil {
		return nil, err
	}
	if frame.DType != contract.TensorDTypeUnspecified && frame.DType != contract.TensorDTypeF32 {
		return nil, fmt.Errorf("inference: decode f32 payload: dtype %d", frame.DType)
	}
	n, err := frame.PayloadElementCount()
	if err != nil {
		return nil, err
	}
	out := make([]float32, n)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(frame.Payload[i*4:]))
	}
	return out, nil
}

func shapeVolume(shape []uint32) (uint64, error) {
	if len(shape) == 0 {
		return 0, fmt.Errorf("inference: activation shape required")
	}
	var n uint64 = 1
	for _, d := range shape {
		n *= uint64(d)
	}
	if n == 0 {
		return 0, fmt.Errorf("inference: activation shape has zero volume")
	}
	return n, nil
}

// ResolvePipelineInput normalizes optional pipeline input bytes into a contract
// ActivationFrame plus raw payload bytes for the data plane.
func ResolvePipelineInput(input []byte) (contract.ActivationFrame, []byte, error) {
	if len(input) == 0 {
		input = EncodeActivation(DefaultActivation)
	}
	act, err := ActivationFromBytes(input, nil, TensorDTypeF32)
	if err != nil {
		return contract.ActivationFrame{}, nil, err
	}
	frame, err := ToContractFrame(act)
	if err != nil {
		return contract.ActivationFrame{}, nil, err
	}
	return frame, act.PayloadOnly(), nil
}

// DecodePipelineActivation parses inbound bytes plus optional structured metadata.
func DecodePipelineActivation(payload []byte, frame contract.ActivationFrame) (Activation, error) {
	if len(frame.Payload) > 0 || len(frame.Shape) > 0 {
		return ActivationFromContractFrame(frame)
	}
	if len(payload) > 0 {
		if act, err := ActivationFromWire(payload); err == nil {
			return act, nil
		}
		return ActivationFromBytes(payload, nil, TensorDTypeF32)
	}
	return Activation{}, fmt.Errorf("inference: pipeline activation missing payload")
}

// ActivationFromContractFrame converts a contract frame to an inference Activation.
func ActivationFromContractFrame(frame contract.ActivationFrame) (Activation, error) {
	var dtype TensorDType
	switch frame.DType {
	case contract.TensorDTypeF32, contract.TensorDTypeUnspecified:
		dtype = TensorDTypeF32
	default:
		return Activation{}, fmt.Errorf("inference: unsupported contract dtype %d", frame.DType)
	}
	return ActivationFromBytes(frame.Payload, frame.Shape, dtype)
}

// ToContractFrame converts an inference Activation to a contract ActivationFrame.
func ToContractFrame(act Activation) (contract.ActivationFrame, error) {
	if err := act.normalize(); err != nil {
		return contract.ActivationFrame{}, err
	}
	var dtype contract.TensorDType
	switch act.DType {
	case TensorDTypeF32, "":
		dtype = contract.TensorDTypeF32
	default:
		return contract.ActivationFrame{}, fmt.Errorf("inference: unsupported activation dtype %q", act.DType)
	}
	return contract.ActivationFrame{
		Payload:     act.PayloadOnly(),
		Shape:       append([]uint32(nil), act.Shape...),
		DType:       dtype,
		Compression: contract.CompressionNone,
	}, nil
}

// MarshalPipelineActivation encodes a frame for QUIC transfer between nodes.
func MarshalPipelineActivation(frame contract.ActivationFrame) ([]byte, error) {
	return contract.MarshalActivationFrame(frame)
}

// ActivationMetadata returns shape/dtype metadata with payload cleared (payload
// rides the data plane separately in v0.1).
func ActivationMetadata(frame contract.ActivationFrame) contract.ActivationFrame {
	meta := frame
	meta.Payload = nil
	return meta
}

// EncodePipelineOutput serializes shard output for the next pipeline stage.
// Fixed split-MLP vectors stay as raw 16-byte payloads; variable tensors are
// wrapped as versioned ActivationFrame wire bytes.
func EncodePipelineOutput(act Activation) ([]byte, error) {
	if err := act.normalize(); err != nil {
		return nil, err
	}
	if act.IsSplitMLPFixed() {
		return act.PayloadOnly(), nil
	}
	frame, err := ToContractFrame(act)
	if err != nil {
		return nil, err
	}
	return contract.MarshalActivationFrame(frame)
}

// WrapStageOutput tags shard output bytes for the next pipeline stage.
func WrapStageOutput(output []byte, prior contract.ActivationFrame) (contract.ActivationFrame, error) {
	if act, err := ActivationFromWire(output); err == nil {
		return ToContractFrame(act)
	}
	if len(output) == ActivationBytes {
		return contract.ActivationFrameFromF32Bytes(SplitMLPShape, output)
	}
	if len(prior.Shape) > 0 {
		vol, err := shapeVolume(prior.Shape)
		if err == nil && uint64(len(output)) == vol*4 {
			return contract.ActivationFrameFromF32Bytes(prior.Shape, output)
		}
	}
	return contract.ActivationFrame{}, fmt.Errorf("inference: cannot infer activation shape from %d-byte output", len(output))
}

// ActivationFromWire parses versioned ActivationFrame bytes or legacy raw split-MLP.
func ActivationFromWire(b []byte) (Activation, error) {
	frame, err := contract.UnmarshalActivationFrame(b)
	if err != nil {
		return Activation{}, err
	}
	return ActivationFromContractFrame(frame)
}

// UnmarshalPipelineActivation decodes QUIC/dataplane activation wire bytes.
func UnmarshalPipelineActivation(raw []byte) (contract.ActivationFrame, error) {
	return contract.UnmarshalActivationFrame(raw)
}

// ForwardActivationFrame runs ForwardActivation on a contract frame.
func ForwardActivationFrame(backend Backend, frame contract.ActivationFrame, layerLo, layerHi uint32) (contract.ActivationFrame, string, error) {
	act, err := ActivationFromContractFrame(frame)
	if err != nil {
		return contract.ActivationFrame{}, "", err
	}
	out, reported, err := ForwardActivation(backend, act, layerLo, layerHi)
	if err != nil {
		return contract.ActivationFrame{}, "", err
	}
	outFrame, err := ToContractFrame(out)
	if err != nil {
		return contract.ActivationFrame{}, "", err
	}
	return outFrame, reported, nil
}
