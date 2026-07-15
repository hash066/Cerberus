package contract

import (
	"encoding/binary"
	"fmt"
	"math"
)

// activationWireVersion is prepended to MarshalActivationFrame output so future
// encodings can be distinguished without breaking v0.1 readers.
const activationWireVersion byte = 1

// ActivationFrameF32 builds a none-compressed f32 activation frame.
func ActivationFrameF32(shape []uint32, values []float32) (ActivationFrame, error) {
	if len(shape) == 0 {
		return ActivationFrame{}, fmt.Errorf("contract: activation shape required")
	}
	var want uint64 = 1
	for _, d := range shape {
		want *= uint64(d)
	}
	if want == 0 {
		return ActivationFrame{}, fmt.Errorf("contract: activation shape has zero volume")
	}
	if uint64(len(values)) != want {
		return ActivationFrame{}, fmt.Errorf("contract: activation shape %v wants %d f32 values, got %d", shape, want, len(values))
	}
	payload := make([]byte, len(values)*4)
	for i, v := range values {
		binary.LittleEndian.PutUint32(payload[i*4:], math.Float32bits(v))
	}
	return ActivationFrame{
		Payload:     payload,
		Shape:       append([]uint32(nil), shape...),
		DType:       TensorDTypeF32,
		Compression: CompressionNone,
	}, nil
}

// ActivationFrameFromF32Bytes wraps an existing little-endian f32 payload.
func ActivationFrameFromF32Bytes(shape []uint32, payload []byte) (ActivationFrame, error) {
	n, err := shapeVolume(shape)
	if err != nil {
		return ActivationFrame{}, err
	}
	if uint64(len(payload)) != n*4 {
		return ActivationFrame{}, fmt.Errorf("contract: f32 payload wants %d bytes, got %d", n*4, len(payload))
	}
	return ActivationFrame{
		Payload:     append([]byte(nil), payload...),
		Shape:       append([]uint32(nil), shape...),
		DType:       TensorDTypeF32,
		Compression: CompressionNone,
	}, nil
}

func shapeVolume(shape []uint32) (uint64, error) {
	if len(shape) == 0 {
		return 0, fmt.Errorf("contract: activation shape required")
	}
	var n uint64 = 1
	for _, d := range shape {
		n *= uint64(d)
	}
	if n == 0 {
		return 0, fmt.Errorf("contract: activation shape has zero volume")
	}
	return n, nil
}

// PayloadElementCount returns the tensor volume implied by Shape.
func (f ActivationFrame) PayloadElementCount() (uint64, error) {
	return shapeVolume(f.Shape)
}

// Validate checks dtype/shape against payload length.
func (f ActivationFrame) Validate() error {
	if len(f.Payload) == 0 {
		return fmt.Errorf("contract: activation missing payload")
	}
	n, err := f.PayloadElementCount()
	if err != nil {
		return err
	}
	switch f.DType {
	case TensorDTypeUnspecified, TensorDTypeF32:
		if uint64(len(f.Payload)) != n*4 {
			return fmt.Errorf("contract: f32 activation wants %d bytes, got %d", n*4, len(f.Payload))
		}
	case TensorDTypeF16, TensorDTypeBF16:
		if uint64(len(f.Payload)) != n*2 {
			return fmt.Errorf("contract: f16/bf16 activation wants %d bytes, got %d", n*2, len(f.Payload))
		}
	case TensorDTypeI8:
		if uint64(len(f.Payload)) != n {
			return fmt.Errorf("contract: i8 activation wants %d bytes, got %d", n, len(f.Payload))
		}
	default:
		return fmt.Errorf("contract: unknown activation dtype %d", f.DType)
	}
	if f.Compression == CompressionFrontierZK {
		// Frontier stub: tagged on the wire but not decodable in v0.1.
		return fmt.Errorf("contract: COMPRESSION_FRONTIER_ZK is not implemented")
	}
	return nil
}

// MarshalActivationFrame encodes f as versioned protobuf-compatible bytes
// mirroring proto/cerberus/v1/compute.proto ActivationFrame field layout.
func MarshalActivationFrame(f ActivationFrame) ([]byte, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	var out []byte
	if len(f.Payload) > 0 {
		out = appendProtoBytes(out, 1, f.Payload)
	}
	for _, dim := range f.Shape {
		out = appendProtoUint32(out, 2, dim)
	}
	if f.DType != TensorDTypeUnspecified {
		out = appendProtoVarint(out, 3, uint64(f.DType))
	}
	if f.Compression != CompressionUnspecified {
		out = appendProtoVarint(out, 4, uint64(f.Compression))
	}
	if f.StageIndex != 0 {
		out = appendProtoUint32(out, 5, f.StageIndex)
	}
	prefix := []byte{activationWireVersion}
	prefix = appendVarint(prefix, uint64(len(out)))
	return append(prefix, out...), nil
}

// UnmarshalActivationFrame decodes bytes produced by MarshalActivationFrame.
func UnmarshalActivationFrame(b []byte) (ActivationFrame, error) {
	if len(b) == 0 {
		return ActivationFrame{}, fmt.Errorf("contract: empty activation frame")
	}
	if b[0] != activationWireVersion {
		return ActivationFrame{}, fmt.Errorf("contract: unsupported activation wire version %d", b[0])
	}
	n, rest, err := readVarint(b[1:])
	if err != nil {
		return ActivationFrame{}, err
	}
	if n > uint64(len(rest)) {
		return ActivationFrame{}, fmt.Errorf("contract: truncated activation frame")
	}
	body := rest[:n]
	rest = rest[n:]

	var f ActivationFrame
	for len(body) > 0 {
		key, rem, err := readVarint(body)
		if err != nil {
			return ActivationFrame{}, err
		}
		field := int(key >> 3)
		wire := int(key & 0x7)
		switch field {
		case 1: // payload
			if wire != 2 {
				return ActivationFrame{}, fmt.Errorf("contract: bad wire type for payload")
			}
			l, tail, err := readVarint(rem)
			if err != nil {
				return ActivationFrame{}, err
			}
			if l > uint64(len(tail)) {
				return ActivationFrame{}, fmt.Errorf("contract: truncated payload")
			}
			f.Payload = append([]byte(nil), tail[:l]...)
			body = tail[l:]
		case 2: // shape
			if wire != 0 {
				return ActivationFrame{}, fmt.Errorf("contract: bad wire type for shape")
			}
			dim, tail, err := readVarint(rem)
			if err != nil {
				return ActivationFrame{}, err
			}
			f.Shape = append(f.Shape, uint32(dim))
			body = tail
		case 3: // dtype
			if wire != 0 {
				return ActivationFrame{}, fmt.Errorf("contract: bad wire type for dtype")
			}
			d, tail, err := readVarint(rem)
			if err != nil {
				return ActivationFrame{}, err
			}
			f.DType = TensorDType(d)
			body = tail
		case 4: // compression
			if wire != 0 {
				return ActivationFrame{}, fmt.Errorf("contract: bad wire type for compression")
			}
			c, tail, err := readVarint(rem)
			if err != nil {
				return ActivationFrame{}, err
			}
			f.Compression = CompressionHint(c)
			body = tail
		case 5: // stage_index
			if wire != 0 {
				return ActivationFrame{}, fmt.Errorf("contract: bad wire type for stage_index")
			}
			s, tail, err := readVarint(rem)
			if err != nil {
				return ActivationFrame{}, err
			}
			f.StageIndex = uint32(s)
			body = tail
		default:
			var skipErr error
			body, skipErr = skipProtoField(wire, rem, body)
			if skipErr != nil {
				return ActivationFrame{}, skipErr
			}
		}
	}
	if len(rest) != 0 {
		return ActivationFrame{}, fmt.Errorf("contract: trailing bytes in activation frame")
	}
	if err := f.Validate(); err != nil {
		return ActivationFrame{}, err
	}
	return f, nil
}

// TaskActivation returns the structured activation on a task, if any.
func TaskActivation(t ComputeTask) (ActivationFrame, bool) {
	if len(t.Activation.Payload) > 0 {
		return t.Activation, true
	}
	return ActivationFrame{}, false
}

func appendVarint(dst []byte, v uint64) []byte {
	for v >= 0x80 {
		dst = append(dst, byte(v)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}

func appendProtoBytes(dst []byte, field int, b []byte) []byte {
	key := uint64(field<<3 | 2)
	dst = appendVarint(dst, key)
	dst = appendVarint(dst, uint64(len(b)))
	return append(dst, b...)
}

func appendProtoUint32(dst []byte, field int, v uint32) []byte {
	return appendProtoVarint(dst, field, uint64(v))
}

func appendProtoVarint(dst []byte, field int, v uint64) []byte {
	key := uint64(field<<3 | 0)
	dst = appendVarint(dst, key)
	return appendVarint(dst, v)
}

func readVarint(b []byte) (uint64, []byte, error) {
	var x uint64
	var s uint
	for i, c := range b {
		if c < 0x80 {
			if i > 9 || (i == 9 && c > 1) {
				return 0, nil, fmt.Errorf("contract: varint overflow")
			}
			return x | uint64(c)<<s, b[i+1:], nil
		}
		x |= uint64(c&0x7f) << s
		s += 7
	}
	return 0, nil, fmt.Errorf("contract: truncated varint")
}

func skipProtoField(wire int, v []byte, body []byte) ([]byte, error) {
	switch wire {
	case 0:
		_, tail, err := readVarint(v)
		return tail, err
	case 1:
		if len(v) < 8 {
			return nil, fmt.Errorf("contract: truncated fixed64")
		}
		return v[8:], nil
	case 2:
		l, tail, err := readVarint(v)
		if err != nil {
			return nil, err
		}
		if l > uint64(len(tail)) {
			return nil, fmt.Errorf("contract: truncated length-delimited field")
		}
		return tail[l:], nil
	case 5:
		if len(v) < 4 {
			return nil, fmt.Errorf("contract: truncated fixed32")
		}
		return v[4:], nil
	default:
		return nil, fmt.Errorf("contract: unsupported wire type %d", wire)
	}
}
