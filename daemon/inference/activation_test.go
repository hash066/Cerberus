package inference

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"math"
	"testing"
)

func TestActivationFromSplitMLP(t *testing.T) {
	a := ActivationFromSplitMLP(DefaultActivation)
	if !a.IsSplitMLPFixed() {
		t.Fatal("expected split-MLP fixed activation")
	}
	got, err := a.AsSplitMLP()
	if err != nil {
		t.Fatal(err)
	}
	if got != DefaultActivation {
		t.Fatalf("got %v want %v", got, DefaultActivation)
	}
}

func TestActivationVariableHiddenRoundTrip(t *testing.T) {
	shape := []uint32{2, 3}
	values := []float32{1, 2, 3, 4, 5, 6}
	payload := make([]byte, len(values)*4)
	for i, v := range values {
		binary.LittleEndian.PutUint32(payload[i*4:], math.Float32bits(v))
	}
	a, err := ActivationFromBytes(payload, shape, TensorDTypeF32)
	if err != nil {
		t.Fatal(err)
	}
	if a.IsSplitMLPFixed() {
		t.Fatal("variable activation must not look like split-MLP")
	}
	b64 := a.EncodeActivationBase64()
	got, err := DecodeActivationBase64(b64, shape, TensorDTypeF32)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Payload, payload) {
		t.Fatalf("payload mismatch")
	}
}

func TestActivationBase64RoundTrip(t *testing.T) {
	raw := []byte{0, 0, 128, 63, 0, 0, 0, 64}
	b64 := base64.StdEncoding.EncodeToString(raw)
	got, err := DecodeActivationBase64(b64, []uint32{2}, TensorDTypeF32)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Payload, raw) {
		t.Fatal("payload mismatch")
	}
}

func TestForwardActivationSplitMLPCPU(t *testing.T) {
	in := ActivationFromSplitMLP(DefaultActivation)
	out, reported, err := ForwardActivation(BackendCPUSoftware, in, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if reported != string(BackendCPUSoftware) {
		t.Fatalf("backend = %q", reported)
	}
	want := splitMLPExpectedOutput()
	got, err := out.AsSplitMLP()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("output mismatch: got %v want %v", got, want)
	}
}

func TestHiddenStateF32ContractRoundTrip(t *testing.T) {
	cfg := ModelActivationConfig{BatchSize: 1, SeqLen: 4, HiddenSize: 8}
	values := make([]float32, 32)
	for i := range values {
		values[i] = float32(i) * 0.25
	}
	frame, err := EncodeHiddenStateF32(cfg, values)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := MarshalPipelineActivation(frame)
	if err != nil {
		t.Fatal(err)
	}
	gotFrame, err := UnmarshalPipelineActivation(wire)
	if err != nil {
		t.Fatal(err)
	}
	got, gotCfg, err := DecodeHiddenStateF32(gotFrame)
	if err != nil {
		t.Fatal(err)
	}
	if gotCfg != cfg {
		t.Fatalf("config = %+v want %+v", gotCfg, cfg)
	}
	for i, want := range values {
		if got[i] != want {
			t.Fatalf("value[%d] = %v want %v", i, got[i], want)
		}
	}
}

func TestResolvePipelineInputSplitMLPBackwardCompat(t *testing.T) {
	frame, payload, err := ResolvePipelineInput(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) != ActivationBytes || len(frame.Shape) != 1 || frame.Shape[0] != ActivationDim {
		t.Fatalf("default: frame=%+v payload=%d", frame, len(payload))
	}
	raw := EncodeActivation(DefaultActivation)
	frame, payload, err = ResolvePipelineInput(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) != ActivationBytes {
		t.Fatalf("payload len = %d", len(payload))
	}
	act, err := ActivationFromContractFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if !act.IsSplitMLPFixed() {
		t.Fatal("expected split-MLP frame")
	}
}

func TestWrapStageOutputPreservesHiddenShape(t *testing.T) {
	cfg := ModelActivationConfig{SeqLen: 2, HiddenSize: 3}
	prior, err := EncodeHiddenStateF32(cfg, []float32{1, 2, 3, 4, 5, 6})
	if err != nil {
		t.Fatal(err)
	}
	nextPayload := make([]byte, 2*3*4)
	for i := range 6 {
		binary.LittleEndian.PutUint32(nextPayload[i*4:], math.Float32bits(float32(i+10)))
	}
	next, err := WrapStageOutput(nextPayload, prior)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Shape) != 3 || next.Shape[1] != 2 || next.Shape[2] != 3 {
		t.Fatalf("shape = %v", next.Shape)
	}
}

func TestKVCacheHandleStubStatus(t *testing.T) {
	kv := &KVCacheHandle{Handle: "kv-layer-3", Layer: 3}
	if kv.StubStatus() == "" {
		t.Fatal("expected stub status")
	}
	frame, err := EncodeHiddenStateF32(ModelActivationConfig{SeqLen: 1, HiddenSize: 2}, []float32{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := BundleFromFrame(frame, kv)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.KVCache == nil || bundle.KVCache.Handle != "kv-layer-3" {
		t.Fatalf("bundle = %+v", bundle)
	}
}

func TestActivationMetadataClearsPayload(t *testing.T) {
	frame, err := EncodeHiddenStateF32(ModelActivationConfig{SeqLen: 1, HiddenSize: 2}, []float32{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	meta := ActivationMetadata(frame)
	if len(meta.Payload) != 0 || len(meta.Shape) != 3 {
		t.Fatalf("metadata = %+v", meta)
	}
}
