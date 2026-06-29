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
