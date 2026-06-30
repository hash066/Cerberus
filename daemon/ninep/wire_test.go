package ninep

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/hugelgupf/p9/p9"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
)

// devWalk are the path components from the namespace root (/cer) down to the
// registered VRAM device dir (/cer/dev/vram/AA/0).
var devWalk = []string{"dev", "vram", "AA", "0"}

// wireSetup builds a capability-gated namespace with one VRAM device and mints a
// read+alloc capability for it (mirrors ninep_test.go's setup).
func wireSetup() (*Server, contract.CapHandle, *stub.CapKernel) {
	k := stub.NewCapKernel()
	s := New(k)
	s.Register(dev, vramRef())
	cap, _ := k.Mint(vramRef(), []contract.Right{contract.RightRead, contract.RightAlloc}, nil)
	return s, cap, k
}

// readAll opens an already-walked client file read-only and drains it.
func readAll(t *testing.T, f p9.File) []byte {
	t.Helper()
	if _, _, err := f.Open(p9.ReadOnly); err != nil {
		t.Fatalf("open: %v", err)
	}
	var out []byte
	buf := make([]byte, 256)
	var off int64
	for {
		n, err := f.ReadAt(buf, off)
		out = append(out, buf[:n]...)
		off += int64(n)
		if err == io.EOF || n == 0 {
			break
		}
		if err != nil {
			t.Fatalf("readAt: %v", err)
		}
	}
	return out
}

// walkTo walks from the attached root to root/extra... and returns the file.
func walkTo(t *testing.T, root p9.File, extra ...string) (p9.File, error) {
	t.Helper()
	names := append(append([]string{}, devWalk...), extra...)
	_, f, err := root.Walk(names)
	return f, err
}

// TestWireOpenCtlReturnsEndpoint: attach with a valid cap, walk to the device,
// open ctl → the bytes are the DataEndpoint descriptor (the data-plane handle),
// NOT device bytes.
func TestWireOpenCtlReturnsEndpoint(t *testing.T) {
	s, cap, _ := wireSetup()
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

	ctl, err := walkTo(t, root, "ctl")
	if err != nil {
		t.Fatalf("walk to ctl with valid cap should succeed: %v", err)
	}
	defer ctl.Close()

	raw := readAll(t, ctl)
	var ep DataEndpoint
	if err := json.Unmarshal(raw, &ep); err != nil {
		t.Fatalf("ctl must yield a DataEndpoint descriptor, got %q: %v", raw, err)
	}
	if ep.Kind != EndpointQUIC || ep.StreamID == 0 {
		t.Fatalf("expected a data-plane endpoint, got %+v", ep)
	}
	if ep.Quota.Bytes != 2*1024*1024*1024 {
		t.Fatalf("endpoint must carry the resource quota, got %d", ep.Quota.Bytes)
	}
}

// TestWireInfoIsByteReadable: info is the only byte-readable leaf; it yields the
// static descriptor.
func TestWireInfoIsByteReadable(t *testing.T) {
	s, cap, _ := wireSetup()
	cl, closer, _ := DialCap(s, cap)
	defer closer()
	root, _ := cl.Attach("/")
	defer root.Close()

	info, err := walkTo(t, root, "info")
	if err != nil {
		t.Fatalf("walk to info should succeed: %v", err)
	}
	defer info.Close()
	raw := readAll(t, info)
	if !strings.Contains(string(raw), "vram") {
		t.Fatalf("info descriptor should name the resource kind, got %s", raw)
	}
}

// TestWireOpenCtlWithoutCapDenied: a connection bound to NO capability (handle 0)
// is denied at open of ctl. No ambient authority.
func TestWireOpenCtlWithoutCapDenied(t *testing.T) {
	s, _, _ := wireSetup()
	// Bind the connection to the zero handle — never minted, never valid.
	cl, closer, _ := DialCap(s, contract.CapHandle(0))
	defer closer()
	root, _ := cl.Attach("/")
	defer root.Close()

	// Walking to the device is already denied without a cap (read check).
	if _, err := walkTo(t, root, "ctl"); err == nil {
		t.Fatal("walk/open of ctl without a capability must be denied")
	}
}

// TestWireOpenCtlWithInvalidCapDenied: a connection bound to a revoked cap is
// denied at open of ctl.
func TestWireOpenCtlWithInvalidCapDenied(t *testing.T) {
	s, cap, k := wireSetup()
	_ = k.Revoke(cap) // now invalid

	cl, closer, _ := DialCap(s, cap)
	defer closer()
	root, _ := cl.Attach("/")
	defer root.Close()

	if _, err := walkTo(t, root, "ctl"); err == nil {
		t.Fatal("revoked capability must not reach ctl")
	}
}

// TestWireNonCtlNonInfoLeafDenied: opening "alloc" (a leaf that is neither ctl
// nor info) is denied — only ctl→endpoint and info→bytes are permitted.
func TestWireNonCtlNonInfoLeafDenied(t *testing.T) {
	s, cap, _ := wireSetup()
	cl, closer, _ := DialCap(s, cap)
	defer closer()
	root, _ := cl.Attach("/")
	defer root.Close()

	leaf, err := walkTo(t, root, "alloc")
	if err != nil {
		// Walk may already fail; that is an acceptable denial.
		return
	}
	defer leaf.Close()
	if _, _, err := leaf.Open(p9.ReadOnly); err == nil {
		t.Fatal("opening a non-ctl/non-info leaf must be denied")
	}
}

// TestWireDirectoryNotByteReadable: a directory node (the device dir) cannot be
// opened for bytes — bytes only ever flow from ctl (endpoint) / info.
func TestWireDirectoryNotByteReadable(t *testing.T) {
	s, cap, _ := wireSetup()
	cl, closer, _ := DialCap(s, cap)
	defer closer()
	root, _ := cl.Attach("/")
	defer root.Close()

	devDir, err := walkTo(t, root)
	if err != nil {
		t.Fatalf("walk to device dir with valid cap should succeed: %v", err)
	}
	defer devDir.Close()
	if _, _, err := devDir.Open(p9.ReadOnly); err == nil {
		t.Fatal("opening a directory for bytes must be denied")
	}
}
