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

// lanIP returns this box's default-route source address, or "" if there is no
// usable non-loopback IPv4 route (an offline CI container).
//
// It deliberately does NOT call sourceAddrToward: a test that verifies the
// advertise logic must not derive its expectation from the code under test, or
// it proves only that a function equals itself.
func lanIP(t *testing.T) string {
	t.Helper()
	c, err := net.Dial("udp", "192.0.2.1:9") // TEST-NET-1; routing probe, sends nothing.
	if err != nil {
		return ""
	}
	defer c.Close()
	ua, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok || ua.IP == nil || ua.IP.IsUnspecified() || ua.IP.IsLoopback() {
		return ""
	}
	return ua.IP.String()
}

// TestTransferOverRealLANAddress is the cross-machine proof that loopback cannot
// give. TestAdvertisedAddrOnWildcardBindIsDialable above completes a transfer to
// the advertised address, but on a multi-homed box that is a weaker check than it
// looks: this dev box carries a Hyper-V/WSL vEthernet switch on 192.168.16.1
// alongside the real Wi-Fi LAN on 192.168.0.101, and a transfer to the WSL switch
// address SUCCEEDS from this machine while being unreachable from any other. So
// "the transfer worked" does not by itself mean "a peer could have done that".
//
// This test closes that gap as far as one machine honestly can: it binds the
// server to the box's real default-route LAN address (a concrete, non-loopback
// host — packets traverse that interface's stack rather than taking the loopback
// shortcut) and dials it by that same address. That exercises the whole advertise
// path over an address a peer on the LAN genuinely could dial.
//
// WHAT THIS STILL DOES NOT PROVE: that a SECOND physical machine can reach it.
// That needs a second machine, and nothing on one box can substitute. What it
// does prove is the property the original bug violated — the address handed to a
// peer is concrete, routable, bound to a real interface, and carries real bytes.
func TestTransferOverRealLANAddress(t *testing.T) {
	t.Setenv(EnvAdvertise, "")
	ip := lanIP(t)
	if ip == "" {
		t.Skip("no non-loopback default route on this host; nothing to prove against")
	}

	k := stub.NewCapKernel()
	s := NewServer(k, testNow, newTestIdentity(t))
	// Bind the REAL LAN address, not loopback and not a wildcard.
	if err := s.Listen(net.JoinHostPort(ip, "0")); err != nil {
		t.Fatalf("listen on real LAN addr %s: %v", ip, err)
	}
	defer s.Close()

	sink := &collectSink{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx, sink.sink) }()

	capH, err := k.Mint(contract.ResourceRef{Kind: contract.KindFS, Path: "/lan"},
		[]contract.Right{contract.RightRead, contract.RightWrite}, nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	ep := s.RegisterGrant(11, capH, contract.Quota{Bytes: 1 << 20})

	// A concrete bind is advertised verbatim, so the peer is handed the LAN addr.
	host, _, err := net.SplitHostPort(ep.Addr)
	if err != nil {
		t.Fatalf("advertised %q is not host:port: %v", ep.Addr, err)
	}
	if host != ip {
		t.Fatalf("advertised host = %q, want the bound LAN addr %q", host, ip)
	}
	if pip := net.ParseIP(host); pip == nil || pip.IsLoopback() || pip.IsUnspecified() {
		t.Fatalf("advertised %q is not a concrete routable host", ep.Addr)
	}

	// The advertised IP must belong to an interface that is actually UP. This is
	// the independent check that it is a real NIC address rather than something
	// inferred: it is verified by enumerating interfaces, not by re-asking the
	// routing code under test.
	if !ipIsOnAnUpInterface(t, host) {
		t.Fatalf("advertised %q is not assigned to any UP non-loopback interface", host)
	}

	want := []byte("bytes that crossed over the real LAN interface, not loopback")
	if err := NewClient().SendBytes(context.Background(), ep, want); err != nil {
		t.Fatalf("send to real LAN addr %q: %v", ep.Addr, err)
	}
	sink.mu.Lock()
	got := append([]byte(nil), sink.got...)
	sink.mu.Unlock()
	if string(got) != string(want) {
		t.Fatalf("payload via LAN addr = %q, want %q", got, want)
	}
	t.Logf("transferred %d bytes over real LAN address %s (not loopback)", len(want), ep.Addr)
}

// ipIsOnAnUpInterface reports whether ip is assigned to an UP, non-loopback
// interface on this host.
func ipIsOnAnUpInterface(t *testing.T, ip string) bool {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("enumerate interfaces: %v", err)
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.String() == ip {
				return true
			}
		}
	}
	return false
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
