package system

import (
	"testing"
)

func TestSplitMLPForwardFull(t *testing.T) {
	out, err := (SplitMLP{}).ForwardFull(SplitMLPDefaultInput)
	if err != nil {
		t.Fatal(err)
	}
	want := (SplitMLP{}).ExpectedOutput()
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("dim %d: got %v want %v", i, out[i], want[i])
		}
	}
}

func TestSplitMLPShardRanges(t *testing.T) {
	m := SplitMLP{}
	stage01, err := m.ForwardRange(SplitMLPDefaultInput, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	full, err := m.ForwardFull(SplitMLPDefaultInput)
	if err != nil {
		t.Fatal(err)
	}
	stage23, err := m.ForwardRange(stage01, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	for i := range full {
		if stage23[i] != full[i] {
			t.Fatalf("chained shard mismatch dim %d: got %v want %v", i, stage23[i], full[i])
		}
	}
}

func TestEncodeDecodeActivation(t *testing.T) {
	in := SplitMLPDefaultInput
	b := EncodeActivation(in)
	out, err := DecodeActivation(b)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("round-trip failed: %v vs %v", out, in)
	}
}
