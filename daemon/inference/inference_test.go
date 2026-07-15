package inference

import (
	"strings"
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
		{"splitmlp", BackendCPUSoftware, true},
		{"cpu", BackendCPUSoftware, true},
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

// TestParseBackendRejectsRemovedMocks pins the L0 deletion: asking for llamacpp or
// mlx must FAIL LOUDLY, never silently degrade to the 4-float fixture. A caller
// that requests a real engine and is handed a toy is the exact bug this lane exists
// to remove.
func TestParseBackendRejectsRemovedMocks(t *testing.T) {
	for _, name := range []string{"llamacpp", "llama.cpp", "llama", "ggml", "mlx"} {
		got, err := ParseBackend(name)
		if err == nil {
			t.Fatalf("ParseBackend(%q) = %q, want an error — the mock backends were removed", name, got)
		}
		if got != "" {
			t.Fatalf("ParseBackend(%q) returned backend %q alongside an error; must return zero", name, got)
		}
		if !strings.Contains(err.Error(), "daemon/llama") {
			t.Fatalf("ParseBackend(%q) error should point at the real path (daemon/llama), got: %v", name, err)
		}
	}
}

// TestForwardActivationRejectsUnknownBackend ensures the forward path itself fails
// closed, not just the string parser.
func TestForwardActivationRejectsUnknownBackend(t *testing.T) {
	act := ActivationFromSplitMLP(DefaultActivation)
	if _, _, err := ForwardActivation(Backend("llamacpp"), act, 0, 3); err == nil {
		t.Fatal("ForwardActivation with a removed backend must error, not fall back to the fixture")
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
	tag := ComponentTag(BackendCPUSoftware)
	if BackendFromComponent(tag) != BackendCPUSoftware {
		t.Fatal("round-trip failed")
	}
	if !IsPipelineComponent(tag) {
		t.Fatalf("IsPipelineComponent(%q) = false", tag)
	}
}
