package ninep

import (
	"strings"
	"testing"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
)

func vramRef() contract.ResourceRef {
	q := contract.Quota{Bytes: 2 * 1024 * 1024 * 1024}
	return contract.ResourceRef{Kind: contract.KindVRAM, Path: "/cer/dev/vram/AA/0", Quota: &q}
}

const dev = "/cer/dev/vram/AA/0"

func setup() (*Server, contract.CapHandle, *stub.CapKernel) {
	k := stub.NewCapKernel()
	s := New(k)
	s.Register(dev, vramRef())
	cap, _ := k.Mint(vramRef(), []contract.Right{contract.RightRead, contract.RightAlloc}, nil)
	return s, cap, k
}

func TestWalkRequiresCapability(t *testing.T) {
	s, cap, _ := setup()
	if err := s.Walk(dev, cap); err != nil {
		t.Fatalf("walk with cap should succeed: %v", err)
	}
	if err := s.Walk(dev, 0); err == nil {
		t.Fatal("walk without a capability must be denied")
	}
	if err := s.Walk("/cer/dev/vram/ZZ/9", cap); err == nil {
		t.Fatal("walk to unregistered path must be denied")
	}
}

// TestWalkDeniesPhantomPathsUnderADevice pins a bug found by actually mounting
// the namespace and walking it from a shell: `cat <mnt>/dev/vram/AA/0/secret`
// reported "Is a directory" instead of "No such file or directory".
//
// The cause was that Walk resolved its path with deviceFor, which PREFIX-matches
// (it has to: Open/ReadInfo are handed ".../ctl" and ".../info" and must find
// the owning device). So any invented suffix under a registered device — at any
// depth — matched that device, passed its read check, and resolved. wire.go's
// node.Walk then classified the non-leaf name as a directory, conjuring an
// endless tree of phantom directories under every device. The namespace's only
// real paths are structural ancestors, registered device dirs, their ctl/info
// leaves, and /cer/fs — nothing else may resolve, which is exactly what
// wire.go's default branch already claimed ("Unknown paths are denied here").
//
// This was never an authority leak (the phantom is empty, and ctl/info stay
// gated), but a namespace that answers for paths it does not have is lying, and
// the mount made that lie visible to any shell.
func TestWalkDeniesPhantomPathsUnderADevice(t *testing.T) {
	s, cap, _ := setup()
	for _, phantom := range []string{
		dev + "/secret",           // an invented name directly under a device
		dev + "/secret/deeper",    // ...and at depth
		dev + "/ctl/nested",       // below a real leaf
		"/cer/dev/vram/AA/0extra", // a device-dir prefix that is NOT a path boundary
	} {
		if err := s.Walk(phantom, cap); err == nil {
			t.Errorf("walk to %q must be denied: the namespace has no such path, and resolving it "+
				"conjures a phantom directory through any mount of this namespace", phantom)
		}
	}
	// The real paths must keep resolving exactly as before.
	if err := s.Walk(dev, cap); err != nil {
		t.Fatalf("walk to the registered device itself must still succeed: %v", err)
	}
}

func TestOpenCtlReturnsEndpointNotBytes(t *testing.T) {
	s, cap, _ := setup()
	ep, err := s.Open(dev+"/ctl", cap)
	if err != nil {
		t.Fatalf("open ctl should succeed: %v", err)
	}
	if ep.Kind != EndpointQUIC || ep.StreamID == 0 {
		t.Fatalf("expected a data-plane endpoint, got %+v", ep)
	}
	if ep.Quota.Bytes != 2*1024*1024*1024 {
		t.Fatalf("endpoint must carry the resource quota, got %d", ep.Quota.Bytes)
	}
	// The invariant: ctl is NOT byte-readable; bytes go over the data plane.
	if _, err := s.ReadInfo(dev+"/ctl", cap); err == nil {
		t.Fatal("reading ctl as bytes must be refused")
	}
	// And info is not openable to an endpoint.
	if _, err := s.Open(dev+"/info", cap); err == nil {
		t.Fatal("only .../ctl may be opened to an endpoint")
	}
}

func TestReadInfoDescriptor(t *testing.T) {
	s, cap, _ := setup()
	b, err := s.ReadInfo(dev+"/info", cap)
	if err != nil {
		t.Fatalf("read info should succeed: %v", err)
	}
	if !strings.Contains(string(b), "vram") {
		t.Fatalf("descriptor should name the resource kind, got %s", b)
	}
}

func TestRevocationDeniesAccess(t *testing.T) {
	s, cap, k := setup()
	if err := s.Walk(dev, cap); err != nil {
		t.Fatal(err)
	}
	_ = k.Revoke(cap)
	if err := s.Walk(dev, cap); err == nil {
		t.Fatal("revoked capability must be denied")
	}
	if _, err := s.Open(dev+"/ctl", cap); err == nil {
		t.Fatal("revoked capability must not open ctl")
	}
}

// containsAll reports whether want is a subset of got.
func containsAll(got []string, want ...string) bool {
	set := map[string]bool{}
	for _, g := range got {
		set[g] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

// TestListChildrenAncestorDirsAreTraversableWithoutCap: listing a structural
// ancestor directory (e.g. /cer/dev/vram) needs no capability at all — it
// exposes only path-component names, never a resource — mirroring
// IsAncestorDir's existing no-cap-required behavior for Walk.
func TestListChildrenAncestorDirsAreTraversableWithoutCap(t *testing.T) {
	s, _, _ := setup()
	if got := s.ListChildren("/cer/dev/vram", contract.CapHandle(0)); !containsAll(got, "AA") {
		t.Fatalf("listing an ancestor dir should reveal the next path component even with no cap, got %v", got)
	}
	if got := s.ListChildren("/cer/dev/vram/AA", contract.CapHandle(0)); !containsAll(got, "0") {
		t.Fatalf("listing an ancestor dir should reveal the next path component even with no cap, got %v", got)
	}
}

// TestListChildrenDeviceDirRequiresCapability: listing the device directory
// ITSELF (its ctl/info leaves) is gated by the identical read check Walk
// performs — no ambient authority (CLAUDE.md golden rule 5).
func TestListChildrenDeviceDirRequiresCapability(t *testing.T) {
	s, cap, _ := setup()

	withCap := s.ListChildren(dev, cap)
	if !containsAll(withCap, "ctl", "info") {
		t.Fatalf("a valid read capability should reveal ctl/info leaves, got %v", withCap)
	}

	withoutCap := s.ListChildren(dev, contract.CapHandle(0))
	if len(withoutCap) != 0 {
		t.Fatalf("listing a device dir with no capability must reveal no leaves (no ambient authority), got %v", withoutCap)
	}
}

// TestListChildrenRevokedCapDenied: exactly like Walk, a revoked capability
// stops seeing the device's leaves.
func TestListChildrenRevokedCapDenied(t *testing.T) {
	s, cap, k := setup()
	if got := s.ListChildren(dev, cap); !containsAll(got, "ctl", "info") {
		t.Fatalf("valid cap should list leaves before revocation, got %v", got)
	}
	_ = k.Revoke(cap)
	if got := s.ListChildren(dev, cap); len(got) != 0 {
		t.Fatalf("a revoked capability must not list any leaves, got %v", got)
	}
}
