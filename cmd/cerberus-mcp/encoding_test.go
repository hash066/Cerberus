package main

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestDecodeBase64Variants(t *testing.T) {
	wasmMagic := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}

	cases := map[string]string{
		"std padded":   base64.StdEncoding.EncodeToString(wasmMagic),
		"std unpadded": base64.RawStdEncoding.EncodeToString(wasmMagic),
		"url padded":   base64.URLEncoding.EncodeToString(wasmMagic),
		"url unpadded": base64.RawURLEncoding.EncodeToString(wasmMagic),
	}
	for name, in := range cases {
		got, err := decodeBase64(in)
		if err != nil {
			t.Fatalf("%s: decodeBase64(%q): %v", name, in, err)
		}
		if !bytes.Equal(got, wasmMagic) {
			t.Fatalf("%s: decodeBase64(%q) = %x, want %x", name, in, got, wasmMagic)
		}
	}
}

func TestDecodeBase64Empty(t *testing.T) {
	got, err := decodeBase64("")
	if err != nil {
		t.Fatalf("decodeBase64(\"\"): %v", err)
	}
	if got != nil {
		t.Fatalf("decodeBase64(\"\") = %v, want nil", got)
	}
}

func TestDecodeBase64Invalid(t *testing.T) {
	if _, err := decodeBase64("!!!not-base64!!!"); err == nil {
		t.Fatalf("expected an error for invalid base64")
	}
}
