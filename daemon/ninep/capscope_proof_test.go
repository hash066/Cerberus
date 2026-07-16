package ninep

import (
	"testing"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
)

// TestCapabilityIsScopedToItsResource asserts the project's first golden rule
// (CLAUDE.md: "Capabilities, not identities. No ambient authority anywhere"):
// a capability minted for ONE device must not grant access to a DIFFERENT one.
func TestCapabilityIsScopedToItsResource(t *testing.T) {
	kernel := stub.NewCapKernel()
	ns := New(kernel)

	vram := contract.ResourceRef{Kind: contract.KindVRAM, Path: "/cer/dev/vram/local/0"}
	gpu := contract.ResourceRef{Kind: contract.KindGPU, Path: "/cer/dev/gpu/local/0"}
	ns.Register("/cer/dev/vram/local/0", vram)
	ns.Register("/cer/dev/gpu/local/0", gpu)

	// A capability for the VRAM device ONLY.
	vramCap, err := kernel.Mint(vram, []contract.Right{contract.RightRead, contract.RightAlloc}, nil)
	if err != nil {
		t.Fatalf("mint vram cap: %v", err)
	}

	// It must work on the device it names.
	if err := ns.Walk("/cer/dev/vram/local/0", vramCap); err != nil {
		t.Fatalf("vram cap denied on its OWN device: %v", err)
	}

	// It must NOT work on a device it does not name.
	if err := ns.Walk("/cer/dev/gpu/local/0", vramCap); err == nil {
		t.Fatal("AMBIENT AUTHORITY: a capability minted for /cer/dev/vram/local/0 " +
			"was accepted for /cer/dev/gpu/local/0 — capabilities are not resource-scoped")
	}
}
