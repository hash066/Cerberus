package main

import (
	"reflect"
	"testing"
)

func TestExtractFlag(t *testing.T) {
	got, rest := extractFlag([]string{"status", "--json"}, "--json")
	if !got {
		t.Fatalf("--json not detected")
	}
	if !reflect.DeepEqual(rest, []string{"status"}) {
		t.Fatalf("rest = %v, want [status]", rest)
	}

	got, rest = extractFlag([]string{"nodes"}, "--json")
	if got {
		t.Fatalf("--json falsely detected")
	}
	if !reflect.DeepEqual(rest, []string{"nodes"}) {
		t.Fatalf("rest = %v, want [nodes]", rest)
	}
}

func TestExtractValueFlag(t *testing.T) {
	// space form
	v, rest := extractValueFlag([]string{"run", "--on", "abcd", "file.wasm"}, "--on")
	if v != "abcd" {
		t.Fatalf("value = %q, want abcd", v)
	}
	if !reflect.DeepEqual(rest, []string{"run", "file.wasm"}) {
		t.Fatalf("rest = %v", rest)
	}

	// equals form
	v, rest = extractValueFlag([]string{"caps", "mint", "--subject=agent-7"}, "--subject")
	if v != "agent-7" {
		t.Fatalf("value = %q, want agent-7", v)
	}
	if !reflect.DeepEqual(rest, []string{"caps", "mint"}) {
		t.Fatalf("rest = %v", rest)
	}

	// absent
	v, _ = extractValueFlag([]string{"caps", "list"}, "--subject")
	if v != "" {
		t.Fatalf("value = %q, want empty", v)
	}
}

func TestParseTTL(t *testing.T) {
	cases := map[string]int64{
		"":     0,
		"24h":  86400,
		"30m":  1800,
		"3600": 3600,
	}
	for in, want := range cases {
		got, err := parseTTL(in)
		if err != nil {
			t.Fatalf("parseTTL(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("parseTTL(%q) = %d, want %d", in, got, want)
		}
	}
	if _, err := parseTTL("banana"); err == nil {
		t.Fatalf("parseTTL accepted garbage")
	}
}

func TestSplitCSV(t *testing.T) {
	if got := splitCSV("read, exec ,write"); !reflect.DeepEqual(got, []string{"read", "exec", "write"}) {
		t.Fatalf("splitCSV = %v", got)
	}
	if got := splitCSV(""); got != nil {
		t.Fatalf("splitCSV(empty) = %v, want nil", got)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[uint64]string{
		512:                    "512 B",
		2 * 1024 * 1024 * 1024: "2.0 GiB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Fatalf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}
