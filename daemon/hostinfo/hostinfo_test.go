package hostinfo

import (
	"testing"

	contract "github.com/hash066/cerberus/contract/go"
)

// TestRAMIsReal pins T4: daemon/system.localTelemetry hardcoded
// RAMTotal: 16e9 / RAMFree: 8e9 for every node on every machine. RAM() must
// return this box's actual memory.
//
// We cannot assert an exact figure (it is whatever the dev/CI box has), so we
// assert the properties a real reading has and a hardcoded literal does not.
func TestRAMIsReal(t *testing.T) {
	total, free, ok := RAM()
	if !ok {
		t.Skip("OS did not report memory on this platform; RAM() correctly said so")
	}
	if total == 0 {
		t.Fatal("RAM() ok=true but total=0")
	}
	if free > total {
		t.Fatalf("free (%d) > total (%d)", free, total)
	}
	// The exact literal the fake used. If we ever return precisely this, either
	// the machine is a hilarious coincidence or someone re-hardcoded it.
	if total == 16_000_000_000 && free == 8_000_000_000 {
		t.Fatal("RAM() returned exactly the old hardcoded 16e9/8e9 literals")
	}
	t.Logf("real RAM: total=%.2f GB free=%.2f GB", float64(total)/1e9, float64(free)/1e9)
}

// TestInterfacesNeverGuessAMedium is the honesty guard, and it is the most
// important test in this package.
//
// contract.Medium has no "unknown" value and its ZERO value is MediumWiFi. So an
// interface we cannot classify must come back with MediumKnown=false — never
// with a zero-valued Medium that reads as a WiFi claim. This test also pins rule
// 3: a virtual adapter (Hyper-V/WSL switch, docker0, veth) must never be
// classified, no matter how fast it claims to be.
func TestInterfacesNeverGuessAMedium(t *testing.T) {
	ifaces, err := Interfaces()
	if err != nil {
		t.Fatalf("Interfaces(): %v", err)
	}
	if len(ifaces) == 0 {
		t.Skip("no interfaces enumerated on this platform")
	}
	for _, in := range ifaces {
		if in.Virtual && in.MediumKnown {
			t.Errorf("interface %q is virtual but was classified as medium %v (%.0f Mbps). "+
				"A virtual switch is not a real link — on the Windows rig the WSL vEthernet "+
				"adapter reports 10 Gbps Ethernet and is unreachable from any other machine.",
				in.Name, in.Medium, in.Mbps)
		}
		if in.Loopback && in.MediumKnown {
			t.Errorf("interface %q is loopback but was classified as medium %v", in.Name, in.Medium)
		}
		// Nothing may ever claim RDMA. Real RDMA needs RoCE/iWARP hardware and
		// Windows has no consumer path; MediumRDMATB must stay referenced by zero
		// code (ARCHITECTURE.md §8: RDMA is Frontier).
		if in.MediumKnown && in.Medium == contract.MediumRDMATB {
			t.Errorf("interface %q claims MediumRDMATB — no code may claim RDMA", in.Name)
		}
	}
}

// TestInterfacesReportsThisMachine is a visibility test: it prints what the OS
// actually said, so a human can check the classification against `Get-NetAdapter`
// / `ip link` rather than trusting it. It asserts only that SOMETHING real was
// found (a machine running this test has at least a loopback).
func TestInterfacesReportsThisMachine(t *testing.T) {
	ifaces, err := Interfaces()
	if err != nil {
		t.Fatalf("Interfaces(): %v", err)
	}
	var classified int
	for _, in := range ifaces {
		med := "unknown (omitted from telemetry)"
		if in.MediumKnown {
			classified++
			switch in.Medium {
			case contract.MediumWiFi:
				med = "WiFi"
			case contract.MediumEth:
				med = "Ethernet"
			case contract.MediumThunderbolt:
				med = "Thunderbolt/USB4"
			case contract.MediumRDMATB:
				med = "RDMA-TB (SHOULD NEVER HAPPEN)"
			}
		}
		flags := ""
		if in.Virtual {
			flags += " [virtual]"
		}
		if in.Loopback {
			flags += " [loopback]"
		}
		t.Logf("%-42s medium=%-30s %8.0f Mbps mtu=%d%s", in.Name, med, in.Mbps, in.MTU, flags)
	}
	t.Logf("classified %d/%d interfaces", classified, len(ifaces))
}

// TestInterfaceTowardLoopbackResolves proves the routing-table lookup actually
// maps a destination back to a real enumerated interface. Loopback is the one
// destination guaranteed to exist on any machine running this test.
func TestInterfaceTowardLoopbackResolves(t *testing.T) {
	in, ok := InterfaceToward("127.0.0.1")
	if !ok {
		t.Skip("could not resolve an interface toward loopback on this platform")
	}
	if !in.Loopback {
		t.Fatalf("InterfaceToward(127.0.0.1) resolved to %q (loopback=%v), want the loopback interface",
			in.Name, in.Loopback)
	}
}

// TestUpIsPopulated guards a failure mode the Up field introduces: LinkFor now
// refuses a link whose interface is not Up, so an Up that is ALWAYS false (a
// platform that never populates it) would make LinkFor refuse everything and
// silently return NodeTelemetry.Links to being permanently empty — which is the
// exact bug this package was written to fix, reintroduced from the other side.
//
// Any machine running this test has at least one interface that is up.
func TestUpIsPopulated(t *testing.T) {
	ifaces, err := Interfaces()
	if err != nil {
		t.Fatalf("Interfaces: %v", err)
	}
	for _, in := range ifaces {
		if in.Up {
			return
		}
	}
	t.Fatal("no interface reports Up=true. Either this host genuinely has no live " +
		"interface, or Up is not being populated on this platform — in which case " +
		"LinkFor now refuses every link and NodeTelemetry.Links is empty again")
}

// TestClassifiedMediumDoesNotImplyAUsableLink pins the distinction the Up field
// exists to draw. A medium says what a link IS, not whether it EXISTS: the
// Windows dev rig's Realtek GbE NIC is unplugged (APIPA address, 0 Mbps) and
// still reports a real 802.3 NDIS physical medium, so it classifies as MediumEth
// with MediumKnown=true. Anything choosing a link by medium alone would pick a
// dead NIC.
//
// This asserts the invariant that makes that safe: the routing table never hands
// back a down interface, so the interface chosen toward a reachable destination
// is always Up.
func TestInterfaceTowardReturnsAnUpInterface(t *testing.T) {
	in, ok := InterfaceToward("127.0.0.1")
	if !ok {
		t.Skip("could not resolve an interface toward loopback on this platform")
	}
	if !in.Up {
		t.Fatalf("routing table resolved to interface %q with Up=false; a destination "+
			"that routes must route over a live interface", in.Name)
	}
}

// TestLinkForOmitsUnclassifiableLink pins the contract-gap workaround: LinkFor
// must REFUSE (ok=false) rather than hand back a zero-valued Link, because a
// zero-valued contract.Link says "MediumWiFi" to the scheduler.
func TestLinkForOmitsUnclassifiableLink(t *testing.T) {
	// Loopback is never classified (honesty rule), so it is a guaranteed
	// unclassifiable destination.
	_, ok := LinkFor(contract.PeerID{}, "127.0.0.1", 1.0)
	if ok {
		t.Fatal("LinkFor returned a link for the loopback interface, whose medium is " +
			"deliberately unknown; an unclassified link must be OMITTED, since " +
			"contract.Medium's zero value is MediumWiFi and would misreport the link")
	}
}
