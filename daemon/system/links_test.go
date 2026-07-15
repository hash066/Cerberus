package system

import (
	"context"
	"testing"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
	"github.com/hash066/cerberus/daemon/mesh"
)

// TestLocalMemoryIsRealAndLeavesVRAMAlone pins T4 and its lane boundary at once.
//
// localTelemetry hardcoded RAMTotal: 16e9 / RAMFree: 8e9 on every node. RAM must
// become real — but VRAM belongs to Lane G, so wiring real RAM must not touch it.
func TestLocalMemoryIsRealAndLeavesVRAMAlone(t *testing.T) {
	in := contract.Memory{
		RAMTotal:  16_000_000_000, // the old fake
		RAMFree:   8_000_000_000,  // the old fake
		VRAMTotal: 8_000_000_000,  // Lane G's; must survive untouched
		VRAMFree:  6_000_000_000,  // Lane G's; must survive untouched
		SwapFree:  1234,           // must survive untouched
	}
	got := localMemory(in)

	if got.VRAMTotal != in.VRAMTotal || got.VRAMFree != in.VRAMFree {
		t.Errorf("localMemory changed VRAM (%d/%d -> %d/%d); VRAM is Lane G's and must be left alone",
			in.VRAMTotal, in.VRAMFree, got.VRAMTotal, got.VRAMFree)
	}
	if got.SwapFree != in.SwapFree {
		t.Errorf("localMemory changed SwapFree %d -> %d", in.SwapFree, got.SwapFree)
	}
	if got.RAMTotal == 0 {
		t.Fatal("localMemory returned RAMTotal=0")
	}
	if got.RAMFree > got.RAMTotal {
		t.Fatalf("RAMFree (%d) > RAMTotal (%d)", got.RAMFree, got.RAMTotal)
	}
	// It must be THIS machine's memory, not the literal it replaced. (A box with
	// exactly 16,000,000,000 bytes and exactly 8,000,000,000 free would be a
	// remarkable coincidence; real reads are never that round.)
	if got.RAMTotal == 16_000_000_000 && got.RAMFree == 8_000_000_000 {
		t.Fatal("localMemory returned exactly the old hardcoded literals — RAM is still fake")
	}
	t.Logf("real RAM: total=%.2f GB free=%.2f GB (VRAM left as Lane G set it)",
		float64(got.RAMTotal)/1e9, float64(got.RAMFree)/1e9)
}

// A nil fabric must yield no links rather than panic — the honest "nothing known".
func TestLocalLinksNilFabricIsEmpty(t *testing.T) {
	if l := localLinks(context.Background(), nil); len(l) != 0 {
		t.Fatalf("localLinks(nil) = %v, want empty", l)
	}
}

// TestLocalTelemetryWithLinksIsNonBlockingAndHonest exercises the real seam on a
// real Composed system with no peers.
//
// The assertion that matters: with no peers there must be ZERO links — NOT one
// zero-valued link. contract.Medium's zero value is MediumWiFi and a zero RTT
// reads as a perfect link, so a single defaulted entry would tell the scheduler
// "there is a flawless WiFi peer here", inverting the very signal it is meant to
// provide.
func TestLocalTelemetryWithLinksIsNonBlockingAndHonest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sys, err := Compose(ctx, stub.NewCapKernel(), "links-test", nil)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	fab, ok := sys.Fabric.(*mesh.Fabric)
	if !ok {
		t.Fatalf("sys.Fabric is not *mesh.Fabric (got %T)", sys.Fabric)
	}

	tel := localTelemetryWithLinks(ctx, fab)
	if len(tel.Links) != 0 {
		t.Fatalf("a node with no mesh peers reported %d links (%+v); an unmeasurable "+
			"link must be OMITTED, never defaulted", len(tel.Links), tel.Links)
	}
	if tel.Memory.RAMTotal == 0 {
		t.Error("telemetry carries no real RAM")
	}
	if tel.Compute.PCores == 0 {
		t.Error("telemetry carries no CPU cores")
	}
}
