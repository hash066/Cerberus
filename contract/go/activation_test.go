package contract

import (
	"encoding/binary"
	"math"
	"testing"
)

func TestActivationFrameF32RoundTrip(t *testing.T) {
	shape := []uint32{2, 2}
	values := []float32{1, 2, 3, 4}
	frame, err := ActivationFrameF32(shape, values)
	if err != nil {
		t.Fatal(err)
	}
	frame.StageIndex = 2
	wire, err := MarshalActivationFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalActivationFrame(wire)
	if err != nil {
		t.Fatal(err)
	}
	if got.StageIndex != 2 || got.DType != TensorDTypeF32 || got.Compression != CompressionNone {
		t.Fatalf("metadata mismatch: %+v", got)
	}
	if len(got.Shape) != 2 || got.Shape[0] != 2 || got.Shape[1] != 2 {
		t.Fatalf("shape = %v", got.Shape)
	}
	for i, want := range values {
		gotVal := math.Float32frombits(binary.LittleEndian.Uint32(got.Payload[i*4:]))
		if gotVal != want {
			t.Fatalf("value[%d] = %v want %v", i, gotVal, want)
		}
	}
}

func TestActivationFrameValidateRejectsFrontierZK(t *testing.T) {
	frame, err := ActivationFrameF32([]uint32{1}, []float32{0})
	if err != nil {
		t.Fatal(err)
	}
	frame.Compression = CompressionFrontierZK
	if err := frame.Validate(); err == nil {
		t.Fatal("expected frontier compression to be rejected")
	}
}

func TestInferenceTaskToComputeTask(t *testing.T) {
	act, err := ActivationFrameF32([]uint32{4}, []float32{1, 0, 0, 0})
	if err != nil {
		t.Fatal(err)
	}
	it := InferenceTask{
		Task:          ComputeTask{TaskID: []byte("tid")},
		Activation:    act,
		PipelineStage: 1,
	}
	ct := it.ToComputeTask()
	if ct.PipelineStage != 1 || len(ct.Activation.Payload) != 16 {
		t.Fatalf("projection failed: %+v", ct)
	}
}

func TestTaskActivationPrefersStructuredField(t *testing.T) {
	act, err := ActivationFrameF32([]uint32{2}, []float32{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	task := ComputeTask{Activation: act, Caps: [][]byte{nil, []byte("legacy")}}
	got, ok := TaskActivation(task)
	if !ok || len(got.Payload) != 8 {
		t.Fatalf("TaskActivation = %+v ok=%v", got, ok)
	}
}
