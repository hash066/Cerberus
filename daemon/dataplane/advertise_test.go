package dataplane

import (
	"context"
	"net"
	"testing"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
)

// advertise_test.go pins the T1 fix: the data plane must be able to hand a peer
// an address the PEER can dial. Before this, Compose hardcoded
// dp.Listen("127.0.0.1:0") and every Endpoint named loopback, so a remote peer
// dialed its own machine and the data plane could not cross hosts at all.

func TestListenAddrFromEnvDefaultsToLoopback(t *testing.T) {
	t.Setenv(EnvListen, "")
	if got := ListenAddrFromEnv(); got != defaultListenAddr {
		t.Fatalf("default listen addr = %q, want %q (tests must stay loopback-isolated, "+
			"exactly as meshListenAddrs()'s loopback default does for the mesh)", got, defaultListenAddr)
	}
}

func TestListenAddrFromEnvHonorsOverride(t *testing.T) {
	t.Setenv(EnvListen, "0.0.0.0:45999")
	if got := ListenAddrFromEnv(); got != "0.0.0.0:45999" {
		t.Fatalf("listen addr = %q, want the env override; without this the shipping "+
			"daemon cannot bind a routable address", got)
	}
}

// TestAdvertisedAddrOnWildcardBindIsDialable is THE regression test for the bug.
// A server bound to 0.0.0.0 must advertise a CONCRETE, routable host — not
// "0.0.0.0" (undialable) and not "127.0.0.1" (dials the peer's own machine).
//
// The proof is not a string check: we take the advertised address and actually
// complete a real capability-gated transfer to it. If the advertised host were
// wildcard or loopback-when-it-should-not-be, this would fail or would silently
// prove nothing.
func TestAdvertisedAddrOnWildcardBindIsDialable(t *testing.T) {
	t.Setenv(EnvAdvertise, "")
	k := stub.NewCapKernel()
	priv := newTestIdentity(t)
	s := NewServer(k, testNow, priv)
	if err := s.Listen("0.0.0.0:0"); err != nil {
		t.Fatalf("listen wildcard: %v", err)
	}
	defer s.Close()

	sink := &collectSink{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx, sink.sink) }()

	capH, err := k.Mint(contract.ResourceRef{Kind: contract.KindFS, Path: "/adv"},
		[]contract.Right{contract.RightRead, contract.RightWrite}, nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	ep := s.RegisterGrant(7, capH, contract.Quota{Bytes: 1 << 20})

	host, _, err := net.SplitHostPort(ep.Addr)
	if err != nil {
		t.Fatalf("advertised addr %q is not host:port: %v", ep.Addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		t.Fatalf("advertised host %q is not an IP", host)
	}
	// The two ways this bug manifests.
	if ip.IsUnspecified() {
		t.Fatalf("advertised %q: a wildcard address is undialable by a peer — "+
			"the listener's own Addr() must not be handed out verbatim", ep.Addr)
	}
	if ip.IsLoopback() {
		t.Fatalf("advertised %q: a peer handed a loopback address dials ITS OWN machine. "+
			"This is exactly the bug (system.go's hardcoded dp.Listen(\"127.0.0.1:0\")); "+
			"a wildcard-bound server must advertise a routable host", ep.Addr)
	}

	// And it must actually WORK: complete a real transfer to the advertised
	// address, pinned to the server's real identity.
	want := []byte("bytes that crossed via the advertised address")
	if err := NewClient().SendBytes(context.Background(), ep, want); err != nil {
		t.Fatalf("send to advertised addr %q: %v", ep.Addr, err)
	}
	sink.mu.Lock()
	got := append([]byte(nil), sink.got...)
	sink.mu.Unlock()
	if string(got) != string(want) {
		t.Fatalf("payload via advertised addr = %q, want %q", got, want)
	}
}

// A concrete (non-wildcard) bind is the operator's explicit choice and must be
// advertised verbatim — including loopback, which is what keeps every existing
// loopback test working unchanged.
func TestAdvertisedAddrLeavesConcreteBindAlone(t *testing.T) {
	t.Setenv(EnvAdvertise, "")
	s := NewServer(stub.NewCapKernel(), testNow, newTestIdentity(t))
	if err := s.Listen("127.0.0.1:0"); err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer s.Close()

	if got, want := s.AdvertisedAddr(), s.Addr(); got != want {
		t.Fatalf("AdvertisedAddr() = %q, want the bound addr %q unchanged", got, want)
	}
}

// The operator override must beat the routing-table inference: behind a
// NAT/port-forward or on an overlay, being told is better than any heuristic.
func TestAdvertiseHostEnvOverridesInference(t *testing.T) {
	t.Setenv(EnvAdvertise, "203.0.113.7")
	s := NewServer(stub.NewCapKernel(), testNow, newTestIdentity(t))
	if err := s.Listen("0.0.0.0:0"); err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer s.Close()

	host, _, err := net.SplitHostPort(s.AdvertisedAddr())
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if host != "203.0.113.7" {
		t.Fatalf("advertised host = %q, want the %s override", host, EnvAdvertise)
	}
	// The port must still come from the LIVE listener, not the override.
	_, advPort, _ := net.SplitHostPort(s.AdvertisedAddr())
	_, lnPort, _ := net.SplitHostPort(s.Addr())
	if advPort != lnPort {
		t.Fatalf("advertised port %s != listener port %s", advPort, lnPort)
	}
}

// sourceAddrToward must return the source address the OS would really use, and
// must NOT pick a loopback or wildcard. This is the routing-table query that
// replaces interface guessing — the reason we do not "prefer the fastest link"
// is that on the Windows rig the fastest link by every OS metric is a Hyper-V
// virtual switch that no peer can reach.
func TestSourceAddrTowardDefaultRouteIsRoutable(t *testing.T) {
	got := sourceAddrToward("")
	if got == "" {
		t.Skip("no default route on this host; nothing to assert")
	}
	ip := net.ParseIP(got)
	if ip == nil {
		t.Fatalf("sourceAddrToward(\"\") = %q, not an IP", got)
	}
	if ip.IsUnspecified() || ip.IsLoopback() {
		t.Fatalf("sourceAddrToward(\"\") = %q, want a routable local address", got)
	}
}

// Routing toward an explicit peer must yield the source address for THAT peer's
// route. Toward a loopback destination the kernel picks a loopback source — which
// is the correct answer and proves we are consulting the routing table per
// destination rather than returning one cached guess.
func TestSourceAddrTowardPeerUsesThatPeersRoute(t *testing.T) {
	got := sourceAddrToward("127.0.0.1")
	if got == "" {
		t.Skip("no route to loopback?")
	}
	if ip := net.ParseIP(got); ip == nil || !ip.IsLoopback() {
		t.Fatalf("sourceAddrToward(127.0.0.1) = %q, want the loopback source address "+
			"the routing table would pick for a loopback destination", got)
	}
}
