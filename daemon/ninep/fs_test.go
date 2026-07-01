package ninep

import (
	"encoding/json"
	"testing"

	"github.com/hugelgupf/p9/p9"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
)

// fakeFSStore is a minimal FSStore that records the calls the namespace makes,
// so the ninep-level tests can assert the cap-check + dispatch logic without
// pulling in dfs/dataplane (those are exercised end-to-end in daemon/system).
type fakeFSStore struct {
	writes  []string
	reads   []string
	failGet bool
}

func (f *fakeFSStore) BeginWrite(path string, _ contract.CapHandle, transferID uint64) (DataEndpoint, error) {
	f.writes = append(f.writes, path)
	return DataEndpoint{Kind: EndpointQUIC, Endpoint: "quic://recv", StreamID: transferID, Quota: contract.Quota{Bytes: 1 << 20}}, nil
}

func (f *fakeFSStore) BeginRead(path string, _ contract.CapHandle, _ RecvEndpoint) error {
	f.reads = append(f.reads, path)
	if f.failGet {
		return contract.Errf(contract.ErrDenied, "no such file: "+path)
	}
	return nil
}

func fsSetup() (*Server, *fakeFSStore, contract.CapHandle, *stub.CapKernel) {
	k := stub.NewCapKernel()
	s := New(k)
	store := &fakeFSStore{}
	s.SetFSStore(store)
	cap, _ := k.Mint(contract.ResourceRef{Kind: contract.KindFS, Path: "/cer/fs/x"},
		[]contract.Right{contract.RightRead, contract.RightWrite}, nil)
	return s, store, cap, k
}

// TestFSWalkRequiresCapability: the /cer/fs root is traversable, but walking to a
// file requires a capability (WalkFS uses the read right). No ambient authority.
func TestFSWalkRequiresCapability(t *testing.T) {
	s, _, cap, _ := fsSetup()

	if err := s.WalkFS(FSRoot, cap); err != nil {
		t.Fatalf("walk to /cer/fs root should succeed: %v", err)
	}
	if err := s.WalkFS("/cer/fs/x", cap); err != nil {
		t.Fatalf("walk to a file with a cap should succeed: %v", err)
	}
	if err := s.WalkFS("/cer/fs/x", contract.CapHandle(0)); err == nil {
		t.Fatal("walk to a file without a capability must be denied")
	}
	if err := s.WalkFS("/not/fs/path", cap); err == nil {
		t.Fatal("walk to a non-fs path must be denied")
	}
}

// TestFSOpenWriteDispatch: OpenFSWrite cap-checks (write) then dispatches to the
// store, returning a data-plane endpoint (never bytes).
func TestFSOpenWriteDispatch(t *testing.T) {
	s, store, cap, _ := fsSetup()

	ep, err := s.OpenFSWrite("/cer/fs/x", cap)
	if err != nil {
		t.Fatalf("OpenFSWrite: %v", err)
	}
	if ep.Kind != EndpointQUIC || ep.StreamID == 0 {
		t.Fatalf("write must return a data-plane endpoint, got %+v", ep)
	}
	if len(store.writes) != 1 || store.writes[0] != "/cer/fs/x" {
		t.Fatalf("store should have recorded the write, got %v", store.writes)
	}
	// The root itself is not a file — writing to it is denied.
	if _, err := s.OpenFSWrite(FSRoot, cap); err == nil {
		t.Fatal("writing to the /cer/fs root (not a file) must be denied")
	}
}

// TestFSOpenReadDispatch: OpenFSRead cap-checks (read) then dispatches to the
// store; an unknown path surfaces the store's denial.
func TestFSOpenReadDispatch(t *testing.T) {
	s, store, cap, _ := fsSetup()

	if err := s.OpenFSRead("/cer/fs/x", cap, RecvEndpoint{Kind: EndpointQUIC, Endpoint: "quic://r", StreamID: 1}); err != nil {
		t.Fatalf("OpenFSRead: %v", err)
	}
	if len(store.reads) != 1 || store.reads[0] != "/cer/fs/x" {
		t.Fatalf("store should have recorded the read, got %v", store.reads)
	}

	store.failGet = true
	if err := s.OpenFSRead("/cer/fs/missing", cap, RecvEndpoint{}); err == nil {
		t.Fatal("read of an unknown path must fail")
	}
}

// TestFSRevokedCapDenied: a revoked capability cannot walk, write, or read.
func TestFSRevokedCapDenied(t *testing.T) {
	s, _, cap, k := fsSetup()
	_ = k.Revoke(cap)

	if err := s.WalkFS("/cer/fs/x", cap); err == nil {
		t.Fatal("revoked cap must not walk to a file")
	}
	if _, err := s.OpenFSWrite("/cer/fs/x", cap); err == nil {
		t.Fatal("revoked cap must not open a file for write")
	}
	if err := s.OpenFSRead("/cer/fs/x", cap, RecvEndpoint{}); err == nil {
		t.Fatal("revoked cap must not open a file for read")
	}
}

// TestFSUnwiredReportsPartitioned: with no FSStore installed, /cer/fs open reports
// PARTITIONED — the namespace stays usable for devices, and it does not pretend to
// have a filesystem it lacks (maturity honesty).
func TestFSUnwiredReportsPartitioned(t *testing.T) {
	k := stub.NewCapKernel()
	s := New(k) // no SetFSStore
	cap, _ := k.Mint(contract.ResourceRef{Kind: contract.KindFS, Path: "/cer/fs/x"},
		[]contract.Right{contract.RightRead, contract.RightWrite}, nil)

	_, err := s.OpenFSWrite("/cer/fs/x", cap)
	var ce *contract.CapError
	if !asCapError(err, &ce) || ce.Code != contract.ErrPartitioned {
		t.Fatalf("unwired write should report PARTITIONED, got %v", err)
	}
	err = s.OpenFSRead("/cer/fs/x", cap, RecvEndpoint{})
	if !asCapError(err, &ce) || ce.Code != contract.ErrPartitioned {
		t.Fatalf("unwired read should report PARTITIONED, got %v", err)
	}
}

func asCapError(err error, target **contract.CapError) bool {
	ce, ok := err.(*contract.CapError)
	if ok {
		*target = ce
	}
	return ok
}

// TestWireWalkAndOpenFSFile: over the real 9P2000.L wire, a client bound to a
// valid fs capability walks /cer/fs/x and opens it for write; the descriptor it
// gets back is a DataEndpoint (the data-plane handle), NOT file bytes — upholding
// the invariant that 9P carries no bulk bytes. A capability-less connection cannot
// even walk to the file.
func TestWireWalkAndOpenFSFile(t *testing.T) {
	k := stub.NewCapKernel()
	s := New(k)
	s.SetFSStore(&fakeFSStore{})
	cap, _ := k.Mint(contract.ResourceRef{Kind: contract.KindFS, Path: "/cer/fs/x"},
		[]contract.Right{contract.RightRead, contract.RightWrite}, nil)

	cl, closer, err := DialCap(s, cap)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer closer()
	root, err := cl.Attach("/")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer root.Close()

	_, file, err := root.Walk([]string{"fs", "x"})
	if err != nil {
		t.Fatalf("walk /cer/fs/x with a valid cap should succeed: %v", err)
	}
	defer file.Close()

	if _, _, err := file.Open(p9.ReadWrite); err != nil {
		t.Fatalf("open /cer/fs/x for write should succeed: %v", err)
	}
	// The bytes the client can read from the opened node are the endpoint
	// descriptor (the data-plane handle), never file content.
	buf := make([]byte, 512)
	var raw []byte
	var off int64
	for {
		n, rerr := file.ReadAt(buf, off)
		raw = append(raw, buf[:n]...)
		off += int64(n)
		if n == 0 {
			break
		}
		if rerr != nil {
			break
		}
	}
	var ep DataEndpoint
	if err := json.Unmarshal(raw, &ep); err != nil {
		t.Fatalf("fs write open must yield a DataEndpoint descriptor, got %q: %v", raw, err)
	}
	if ep.Kind != EndpointQUIC || ep.StreamID == 0 {
		t.Fatalf("expected a data-plane endpoint, got %+v", ep)
	}

	// A capability-less connection cannot walk to the file.
	cl0, closer0, _ := DialCap(s, contract.CapHandle(0))
	defer closer0()
	root0, _ := cl0.Attach("/")
	defer root0.Close()
	if _, _, err := root0.Walk([]string{"fs", "x"}); err == nil {
		t.Fatal("walk to /cer/fs/x without a capability must be denied")
	}
}
