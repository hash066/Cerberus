//go:build ffi

package ffi

import (
	"testing"

	contract "github.com/hash066/cerberus/contract/go"
)

// These tests run only under `-tags ffi` and exercise the real Rust SignedKernel
// across the cgo boundary (Ed25519 sign + verify + attenuation + revocation).

func TestRustBackendActive(t *testing.T) {
	if got := Backend(); got != "rust-signed-cabi" {
		t.Fatalf("Backend() = %q, want rust-signed-cabi", got)
	}
}

func TestRustMintVerifyRevoke(t *testing.T) {
	k := NewKernel()
	h, err := k.Mint(
		contract.ResourceRef{Kind: contract.KindVRAM, Quota: &contract.Quota{Bytes: 2 << 30}},
		[]contract.Right{contract.RightRead, contract.RightAlloc},
		nil,
	)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if h == 0 {
		t.Fatal("mint returned zero handle")
	}
	if err := k.Verify(h, contract.Request{Op: "read"}, 0); err != nil {
		t.Fatalf("verify read: %v", err)
	}
	if k.IsRevoked(h) {
		t.Fatal("freshly minted cap reports revoked")
	}
	if err := k.Revoke(h); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := k.Verify(h, contract.Request{Op: "read"}, 0); err == nil {
		t.Fatal("verify after revoke should fail")
	}
	if !k.IsRevoked(h) {
		t.Fatal("revoked cap should report revoked")
	}
}

func TestRustAttenuateDropsRight(t *testing.T) {
	k := NewKernel()
	parent, err := k.Mint(
		contract.ResourceRef{Kind: contract.KindVRAM},
		[]contract.Right{contract.RightRead, contract.RightAlloc},
		nil,
	)
	if err != nil {
		t.Fatalf("mint parent: %v", err)
	}
	child, err := k.Attenuate(parent, []contract.Right{contract.RightAlloc}, nil)
	if err != nil {
		t.Fatalf("attenuate: %v", err)
	}
	if err := k.Verify(child, contract.Request{Op: "read"}, 0); err != nil {
		t.Fatalf("child should keep read: %v", err)
	}
	if err := k.Verify(child, contract.Request{Op: "alloc"}, 0); err == nil {
		t.Fatal("child should have lost alloc right")
	}
	// Revoking the parent must invalidate the child (chain walk).
	if err := k.Revoke(parent); err != nil {
		t.Fatalf("revoke parent: %v", err)
	}
	if err := k.Verify(child, contract.Request{Op: "read"}, 0); err == nil {
		t.Fatal("child should be invalid after parent revoked")
	}
}
