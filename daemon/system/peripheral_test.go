package system

import (
	"context"
	"testing"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
)

func TestPeripheralPoolSnapshotLocal(t *testing.T) {
	kernel := stub.NewCapKernel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sys, err := Compose(ctx, kernel, "peripheral-test", nil)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	defer stopSystem(t, sys, cancel)

	if sys.Pool == nil {
		t.Fatal("expected non-nil Pool after Compose")
	}
	snap := sys.Pool.Snapshot()
	if snap.Self == "" {
		t.Fatal("expected self peer id")
	}
	if len(snap.Peers) < 1 {
		t.Fatalf("expected at least self in peers, got %d", len(snap.Peers))
	}
	if snap.Peers[0].Kind != "self" {
		t.Fatalf("first peer kind = %q, want self", snap.Peers[0].Kind)
	}
	if snap.Peers[0].CPU.Threads == 0 {
		t.Fatal("self node should report CPU threads")
	}
	if !snap.Peers[0].GPU.MeshWorker {
		t.Fatal("expected mesh GPU worker wired on composed system")
	}
	if len(snap.Peers[0].Capabilities) < 4 {
		t.Fatalf("expected 4 peripheral capabilities, got %d", len(snap.Peers[0].Capabilities))
	}
	if snap.Totals.Nodes < 1 {
		t.Fatal("totals should include at least one node")
	}
}

func TestPeripheralPoolPeerTelemetry(t *testing.T) {
	kernel := stub.NewCapKernel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sys, err := Compose(ctx, kernel, "peer-tel", nil)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	defer stopSystem(t, sys, cancel)

	peer := contract.PeerID{9: 9}
	sys.Scheduler.UpdateNode(contract.NodeTelemetry{
		PeerID:  peer,
		Compute: contract.Compute{PCores: 4, ECores: 2},
		Memory:  contract.Memory{VRAMTotal: 4_000_000_000, VRAMFree: 2_000_000_000, RAMFree: 1_000_000_000},
	})

	snap := sys.Pool.Snapshot()
	if snap.Totals.Nodes < 2 {
		t.Fatalf("expected self + peer in totals, got nodes=%d", snap.Totals.Nodes)
	}
	if snap.Totals.VRAMTotalBytes < 4_000_000_000 {
		t.Fatalf("VRAM total should include peer, got %d", snap.Totals.VRAMTotalBytes)
	}
}
