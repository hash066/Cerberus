package mesh

import (
	"context"
	"testing"
	"time"

	"github.com/hash066/cerberus/contract/go/stub"
	"github.com/hash066/cerberus/daemon/wasm"
	"github.com/ipfs/go-cid"
)

// TestRequestComponentOverMesh proves the real p2p fetch path end-to-end: node A
// has a component registered locally (in its ContentStore); node B does not.
// B asks A for the bytes behind the CID over the mesh, and must receive back
// bytes that hash to exactly the CID it asked for.
func TestRequestComponentOverMesh(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	a, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("node a: %v", err)
	}
	defer a.Close()
	b, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("node b: %v", err)
	}
	defer b.Close()
	if err := b.Connect(ctx, a.AddrInfo()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// Only A has the component.
	aStore := wasm.NewContentStore()
	component := []byte{0x00, 0x61, 0x73, 0x6d, 1, 2, 3, 4, 5}
	c, err := aStore.Put(component)
	if err != nil {
		t.Fatalf("seed a's store: %v", err)
	}
	a.ServeComponentFetch(aStore)

	bStore := wasm.NewContentStore()
	if bStore.Has(c) {
		t.Fatal("test setup bug: b already has the component")
	}

	got, err := b.RequestComponent(ctx, a.PeerID(), c)
	if err != nil {
		t.Fatalf("b failed to fetch component from a: %v", err)
	}
	gotCID, err := wasm.ComponentCID(got)
	if err != nil {
		t.Fatalf("hash fetched bytes: %v", err)
	}
	if !gotCID.Equals(c) {
		t.Fatalf("fetched bytes hash to %s, want %s", gotCID, c)
	}

	// Populate b's own store, proving a subsequent local lookup now hits.
	if _, err := bStore.Put(got); err != nil {
		t.Fatalf("populate b's store: %v", err)
	}
	if !bStore.Has(c) {
		t.Fatal("b's store was not populated by the fetched bytes")
	}
}

// TestRequestComponentRejectsTamperedBytes proves the requester-side integrity
// check: if a peer's response carries bytes that do not hash to the requested
// CID, RequestComponent must reject them rather than handing them back to the
// caller. We simulate a tampering peer by wiring a fake stream handler that
// returns the wrong bytes for the requested CID.
func TestRequestComponentRejectsTamperedBytes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	a, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("node a: %v", err)
	}
	defer a.Close()
	b, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("node b: %v", err)
	}
	defer b.Close()
	if err := b.Connect(ctx, a.AddrInfo()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	wanted := []byte("the-real-component-bytes")
	wantedCID, err := wasm.ComponentCID(wanted)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	// A malicious/buggy responder: register a store that reports Has=true for
	// anything but Get returns different bytes than what was asked for.
	a.ServeComponentFetch(tamperingSource{actualBytes: []byte("substituted-bytes")})

	if _, err := b.RequestComponent(ctx, a.PeerID(), wantedCID); err == nil {
		t.Fatal("RequestComponent accepted bytes that do not hash to the requested CID")
	}
}

// TestRequestComponentMissingFailsCleanly proves the failure path: asking a
// peer for a CID it genuinely does not have returns a clear error, not a hang
// or a panic.
func TestRequestComponentMissingFailsCleanly(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping networked mesh test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	a, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("node a: %v", err)
	}
	defer a.Close()
	b, err := New(ctx, Config{Site: "test", Kernel: stub.NewCapKernel()})
	if err != nil {
		t.Fatalf("node b: %v", err)
	}
	defer b.Close()
	if err := b.Connect(ctx, a.AddrInfo()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// A's store is empty — it genuinely has nothing.
	a.ServeComponentFetch(wasm.NewContentStore())

	absentCID, err := wasm.ComponentCID([]byte("nobody-has-this"))
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	done := make(chan struct{})
	var fetchErr error
	go func() {
		defer close(done)
		_, fetchErr = b.RequestComponent(ctx, a.PeerID(), absentCID)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("RequestComponent for a missing CID hung instead of returning an error")
	}
	if fetchErr == nil {
		t.Fatal("RequestComponent for a CID no peer has must fail, not succeed")
	}
}

// tamperingSource implements ComponentSource but always returns actualBytes
// regardless of the CID asked for — simulating a peer that (maliciously or by
// bug) substitutes different bytes for a requested CID, so we can prove
// RequestComponent's integrity check rejects it.
type tamperingSource struct {
	actualBytes []byte
}

func (t tamperingSource) Has(c cid.Cid) bool { return true }
func (t tamperingSource) Get(c cid.Cid) ([]byte, error) {
	return t.actualBytes, nil
}
