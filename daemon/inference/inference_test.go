package inference

import (
	"testing"
)

func TestParseBackend(t *testing.T) {
	cases := []struct {
		in   string
		want Backend
		ok   bool
	}{
		{"", BackendCPUSoftware, true},
		{"cpu-software", BackendCPUSoftware, true},
		{"llamacpp", BackendLlamacpp, true},
		{"llama.cpp", BackendLlamacpp, true},
		{"bogus", "", false},
	}
	for _, tc := range cases {
		got, err := ParseBackend(tc.in)
		if tc.ok && err != nil {
			t.Fatalf("%q: %v", tc.in, err)
		}
		if !tc.ok && err == nil {
			t.Fatalf("%q: expected error", tc.in)
		}
		if tc.ok && got != tc.want {
			t.Fatalf("%q: got %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestSplitMLPForwardMatchesSystemFixture(t *testing.T) {
	out, reported, err := ForwardRange(BackendCPUSoftware, EncodeActivation(DefaultActivation), 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if reported != string(BackendCPUSoftware) {
		t.Fatalf("backend = %q", reported)
	}
	want := splitMLPExpectedOutput()
	got, err := DecodeActivation(out)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("output mismatch: got %v want %v", got, want)
	}
}

func TestComponentTagRoundTrip(t *testing.T) {
	tag := ComponentTag(BackendLlamacpp)
	if BackendFromComponent(tag) != BackendLlamacpp {
		t.Fatal("round-trip failed")
	}
}
