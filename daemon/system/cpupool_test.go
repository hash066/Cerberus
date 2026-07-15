package system

import (
	"testing"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
	"github.com/hash066/cerberus/daemon/ninep"
)

// fakeSnaps is a mutable cpuSnapshotSource so the pool can be tested without a
// full scheduler (and so a peer can be made to "leave" between refreshes).
type fakeSnaps struct{ snaps []contract.NodeTelemetry }

func (f *fakeSnaps) NodeSnapshots() []contract.NodeTelemetry { return f.snaps }

// TestCPUPoolExposesLocalAndRemoteCPU proves /cer/dev/cpu exposes this node's
// and a peer's CPU as capability-gated devices driven by the live telemetry
// view, that opening ctl is capability-checked, and that a peer's device is
// dropped when it leaves the mesh view.
func TestCPUPoolExposesLocalAndRemoteCPU(t *testing.T) {
	kernel := stub.NewCapKernel()
	ns := ninep.New(kernel)

	var self, peer contract.PeerID
	self[0], peer[0] = 1, 2
	src := &fakeSnaps{snaps: []contract.NodeTelemetry{
		{PeerID: self, Compute: contract.Compute{PCores: 4, Flops: 1e12}},
		{PeerID: peer, Compute: contract.Compute{PCores: 8, Flops: 2e12}},
	}}
	cat := &DeviceCatalog{}
	pool := NewCPUPool(ns, src, self, cat)
	pool.refresh()

	// Catalog lists both CPU devices; exactly one is pooled (the peer).
	snap := cat.Snapshot()
	cpuCount, pooledCount := 0, 0
	for _, e := range snap {
		if e.Kind != string(contract.KindCPU) {
			continue
		}
		cpuCount++
		if e.Pooled {
			pooledCount++
		}
	}
	if cpuCount != 2 || pooledCount != 1 {
		t.Fatalf("expected 2 CPU devices (1 pooled), got %d cpu / %d pooled: %+v", cpuCount, pooledCount, snap)
	}

	// A valid minted capability can walk the local CPU device and open its ctl to
	// a data-plane endpoint; an unknown handle is denied (no ambient authority).
	h, err := kernel.Mint(contract.ResourceRef{Kind: contract.KindCPU, Path: "/cer/dev/cpu/local/0"}, nil, nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	localDir := "/cer/dev/cpu/local/0"
	if err := ns.Walk(localDir, h); err != nil {
		t.Fatalf("walk local cpu: %v", err)
	}
	if _, err := ns.Open(localDir+"/ctl", h); err != nil {
		t.Fatalf("open local cpu ctl: %v", err)
	}
	if _, err := ns.Open(localDir+"/ctl", contract.CapHandle(0)); err == nil {
		t.Fatal("expected unknown-capability open of cpu ctl to be denied")
	}

	// The peer's CPU device is present too.
	peerDir := "/cer/dev/cpu/" + peerShortHex(peer) + "/0"
	if err := ns.Walk(peerDir, h); err != nil {
		t.Fatalf("walk remote cpu device: %v", err)
	}

	// Peer leaves the mesh view -> next refresh drops its device; local remains.
	src.snaps = src.snaps[:1]
	pool.refresh()
	if err := ns.Walk(peerDir, h); err == nil {
		t.Fatal("expected remote CPU device to be unregistered after the peer left")
	}
	if err := ns.Walk(localDir, h); err != nil {
		t.Fatalf("local CPU device should survive peer departure: %v", err)
	}
}
