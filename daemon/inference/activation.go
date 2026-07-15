// activation.go defines the v0.1 pipeline activation wire format.
//
// Legacy split-MLP shards use a fixed 4-dimensional little-endian f32 vector.
// Real MLX model shards carry variable-size tensors: token ids (i32) at layer 0
// and hidden states (f32, shape [seq_len, hidden_dim]) between nodes.
package inference

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math"
)

const (
	ActivationDim    = 4
	ActivationBytes  = ActivationDim * 4
	DefaultInputIdx0 = 1 // canonical demo input [1,0,0,0]
)

// TensorDType names the element type of a variable activation payload.
type TensorDType string

const (
	TensorDTypeF32 TensorDType = "f32"
	TensorDTypeI32 TensorDType = "i32"
)

// Activation is bytes plus optional shape/dtype metadata for pipeline shards.
// When Shape is nil and len(Payload)==ActivationBytes the legacy split-MLP
// fixture vector is assumed (f32, shape [4]).
type Activation struct {
	Payload []byte
	Shape   []uint32
	DType   TensorDType
}

// DefaultActivation is the canonical demo input vector [1, 0, 0, 0].
var DefaultActivation = [ActivationDim]float32{1, 0, 0, 0}

// SplitMLPShape is the canonical 1-D shape for the v0.1 split-MLP fixture.
var SplitMLPShape = []uint32{ActivationDim}

// EncodeActivation serializes a 4-vector as little-endian f32 bytes.
func EncodeActivation(v [ActivationDim]float32) []byte {
	b := make([]byte, ActivationBytes)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

// DecodeActivation parses a little-endian f32 activation vector.
func DecodeActivation(b []byte) ([ActivationDim]float32, error) {
	var out [ActivationDim]float32
	if len(b) != ActivationBytes {
		return out, fmt.Errorf("inference: activation must be %d bytes, got %d", ActivationBytes, len(b))
	}
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out, nil
}

// DecodeActivationWire parses pipeline activation bytes. The v0.1 demo uses a
// fixed 4-vector (16 bytes). Real llama.cpp forwards may carry variable-length
// f32 payloads (multiple of 4 bytes) with shape [1, dim].
func DecodeActivationWire(b []byte) (values []float32, shape []uint32, err error) {
	if len(b) == 0 {
		return nil, nil, fmt.Errorf("inference: empty activation")
	}
	if len(b)%4 != 0 {
		return nil, nil, fmt.Errorf("inference: activation payload must be multiple of 4 bytes, got %d", len(b))
	}
	n := len(b) / 4
	values = make([]float32, n)
	for i := range values {
		values[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	if n == ActivationDim {
		shape = []uint32{ActivationDim}
	} else {
		shape = []uint32{1, uint32(n)}
	}
	return values, shape, nil
}

// EncodeActivationWire serializes values with the given shape as little-endian f32.
func EncodeActivationWire(values []float32, shape []uint32) ([]byte, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("inference: activation values required")
	}
	var want uint64 = 1
	for _, d := range shape {
		want *= uint64(d)
	}
	if shape == nil || want == 0 || uint64(len(values)) != want {
		// Fall back to a flat vector when shape metadata is absent.
		if shape != nil && want != 0 && uint64(len(values)) != want {
			return nil, fmt.Errorf("inference: activation shape %v wants %d values, got %d", shape, want, len(values))
		}
	}
	out := make([]byte, len(values)*4)
	for i, f := range values {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(f))
	}
	return out, nil
}

// PipelineActivationDim returns the element count for a wire activation payload.
func PipelineActivationDim(b []byte) (int, error) {
	if len(b)%4 != 0 {
		return 0, fmt.Errorf("inference: activation payload must be multiple of 4 bytes, got %d", len(b))
	}
	return len(b) / 4, nil
}

// ActivationFromSplitMLP wraps the legacy fixed fixture as an Activation.
func ActivationFromSplitMLP(v [ActivationDim]float32) Activation {
	return Activation{
		Payload: EncodeActivation(v),
		Shape:   append([]uint32(nil), SplitMLPShape...),
		DType:   TensorDTypeF32,
	}
}

// ActivationFromBytes builds an Activation from raw payload bytes and optional
// metadata. Nil shape with ActivationBytes payload selects the split-MLP fixture.
func ActivationFromBytes(payload []byte, shape []uint32, dtype TensorDType) (Activation, error) {
	a := Activation{
		Payload: append([]byte(nil), payload...),
		Shape:   append([]uint32(nil), shape...),
		DType:   dtype,
	}
	if err := a.normalize(); err != nil {
		return Activation{}, err
	}
	return a, nil
}

// IsSplitMLPFixed reports whether a carries the legacy 4-dim fixture.
func (a Activation) IsSplitMLPFixed() bool {
	return len(a.Shape) == 1 && a.Shape[0] == ActivationDim &&
		len(a.Payload) == ActivationBytes &&
		(a.DType == "" || a.DType == TensorDTypeF32)
}

func (a *Activation) normalize() error {
	if len(a.Shape) == 0 && len(a.Payload) == ActivationBytes {
		a.Shape = append([]uint32(nil), SplitMLPShape...)
		if a.DType == "" {
			a.DType = TensorDTypeF32
		}
	}
	if a.DType == "" {
		a.DType = TensorDTypeF32
	}
	return a.Validate()
}

// Validate checks dtype, shape, and payload length consistency.
func (a Activation) Validate() error {
	if len(a.Payload) == 0 {
		return fmt.Errorf("inference: activation missing payload")
	}
	bpe, err := a.bytesPerElement()
	if err != nil {
		return err
	}
	if len(a.Shape) == 0 {
		return fmt.Errorf("inference: activation shape required")
	}
	var vol uint64 = 1
	for _, d := range a.Shape {
		if d == 0 {
			return fmt.Errorf("inference: activation shape has zero dimension")
		}
		vol *= uint64(d)
	}
	want := int(vol) * bpe
	if len(a.Payload) != want {
		return fmt.Errorf("inference: activation shape %v wants %d bytes, got %d", a.Shape, want, len(a.Payload))
	}
	return nil
}

func (a Activation) bytesPerElement() (int, error) {
	switch a.DType {
	case TensorDTypeF32:
		return 4, nil
	case TensorDTypeI32:
		return 4, nil
	default:
		return 0, fmt.Errorf("inference: unsupported activation dtype %q", a.DType)
	}
}

// AsSplitMLP decodes the legacy fixed fixture vector.
func (a Activation) AsSplitMLP() ([ActivationDim]float32, error) {
	if !a.IsSplitMLPFixed() {
		return [ActivationDim]float32{}, fmt.Errorf("inference: not a split-MLP activation (shape=%v len=%d)", a.Shape, len(a.Payload))
	}
	return DecodeActivation(a.Payload)
}

// PayloadOnly returns raw tensor bytes for the data plane (no metadata).
func (a Activation) PayloadOnly() []byte {
	return append([]byte(nil), a.Payload...)
}

// EncodeActivationBase64 base64-encodes the payload for the MLX sidecar protocol.
func (a Activation) EncodeActivationBase64() string {
	return base64.StdEncoding.EncodeToString(a.Payload)
}

// DecodeActivationBase64 builds an Activation from a sidecar response.
func DecodeActivationBase64(b64 string, shape []uint32, dtype TensorDType) (Activation, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return Activation{}, fmt.Errorf("inference: decode activation base64: %w", err)
	}
	return ActivationFromBytes(raw, shape, dtype)
}
