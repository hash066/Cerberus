// Package hostinfo samples what the LOCAL MACHINE actually is: how much physical
// memory it has, and what its network interfaces really are (medium, link speed,
// MTU). It exists to replace hardcoded literals in the telemetry the placement
// brain runs on.
//
// Two things were fake or missing before this package:
//
//   - daemon/system.localTelemetry hardcoded RAMTotal: 16e9, RAMFree: 8e9. Every
//     node in the mesh advertised "16 GB, half free" regardless of the machine.
//     RAM() makes that real. (VRAM stays Lane G's; this package does not touch it.)
//   - contract.NodeTelemetry.Links was declared (contract/go/telemetry.go) and
//     CONSUMED (daemon/scheduler penalizes the worst link RTT), but never
//     populated anywhere — `Links:` had zero hits in non-test code, so the
//     scheduler's RTT penalty was permanently zero. Interfaces() + LinkFor()
//     provide the real per-peer data to populate it.
//
// HONESTY RULES THIS PACKAGE FOLLOWS — read before extending it:
//
//  1. NEVER GUESS A MEDIUM. contract.Medium has no "unknown" value AND its zero
//     value is MediumWiFi (contract/go/telemetry.go:9-16), so a struct we fail to
//     fill does not read as "unknown", it reads as "WiFi". That is a trap: an
//     unclassifiable interface silently becomes a WiFi claim. Interface therefore
//     carries MediumKnown, and callers MUST omit a link whose medium is unknown
//     rather than emit a default. See the contract gap flagged in the lane report.
//  2. NO FAKE RDMA. MediumRDMATB is never returned by anything here, and never
//     should be. Real RDMA needs RoCE/iWARP NICs; Windows has no consumer RDMA
//     path at all. Thunderbolt/USB4 DETECTION is real and useful; calling it RDMA
//     would be a lie. RDMA stays Frontier (ARCHITECTURE.md §8).
//  3. A VIRTUAL ADAPTER IS NOT A FAST LINK. On the Windows dev rig the single
//     fastest interface by every OS metric is a Hyper-V/WSL virtual switch
//     reporting 10 Gbps Ethernet — and it is unreachable from any other machine.
//     Interfaces() marks these Virtual and leaves their medium unknown, because
//     "10 Gbps Ethernet" is a true statement about a link to nowhere.
//  4. Unsupported platform => honest zero/empty, never a plausible-looking
//     invention. Same posture as daemon/system/cpuload.go, whose per-OS split
//     this package mirrors.
package hostinfo

import (
	"net"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/pbnjay/memory"
)

// Interface is one local network interface, with its medium classified only when
// the OS actually told us enough to be sure.
type Interface struct {
	// Name is the OS-level interface name / friendly name.
	Name string
	// Addrs are the unicast IPs bound to this interface.
	Addrs []net.IP
	// Medium is the classified interconnect medium. It is meaningful ONLY when
	// MediumKnown is true — the zero value is MediumWiFi, not "unknown".
	Medium contract.Medium
	// MediumKnown reports whether Medium was positively determined from the OS.
	// When false, a caller populating contract telemetry MUST omit the link
	// entirely; there is no contract value meaning "unknown" to report instead.
	MediumKnown bool
	// Mbps is the OS-reported link speed in megabits/sec (0 = unknown). This is
	// the NEGOTIATED link rate the driver advertises, not measured throughput —
	// a 1 GbE NIC reports 1000 whether or not it can push that.
	Mbps float64
	// MTU is the interface MTU (0 = unknown).
	MTU uint32
	// Virtual marks an interface the OS describes as having no real physical
	// medium (Hyper-V/WSL switches, tunnels). These are excluded from medium
	// classification: see honesty rule 3 above.
	//
	// KNOWN FALSE NEGATIVE (measured on the Windows dev rig): this is inferred
	// from the NDIS physical medium being "Unspecified", which catches Hyper-V/WSL
	// switches but NOT a Microsoft Wi-Fi Direct Virtual Adapter — that reports a
	// real Native802.11 medium, so it lands here as Virtual=false even though
	// Get-NetAdapter reports Virtual=True for it. The medium it gets (WiFi) is
	// still truthful, since it rides the real radio, and such adapters are
	// Disconnected so the routing table never selects them — but do not read
	// Virtual==false as "the OS calls this physical".
	Virtual bool
	// Loopback marks the loopback interface.
	Loopback bool
	// Up reports whether the OS considers the interface operationally UP. A
	// classified medium says what a link IS, not whether it EXISTS: the dev rig's
	// Realtek GbE NIC is unplugged and still reports a real 802.3 physical medium,
	// so it classifies as MediumEth with MediumKnown=true while carrying no
	// traffic at all. Callers choosing a link MUST check this; LinkFor does.
	Up bool
}

// RAM returns this machine's real total and available physical memory in bytes.
// ok is false when the OS did not answer, in which case the caller must keep
// whatever it had rather than substitute a guess.
//
// Backed by github.com/pbnjay/memory, which was ALREADY in the module graph (an
// indirect dependency of libp2p's resource manager), so this adds no new
// third-party code to the build — it only promotes an existing dependency to
// direct. It reads GlobalMemoryStatusEx on Windows, /proc/meminfo on Linux, and
// sysctl on darwin/BSD: real OS calls on every platform this daemon targets, with
// no cgo.
//
// "Free" is the OS's AVAILABLE physical memory, which is the number a scheduler
// wants (it counts reclaimable cache as available), not the smaller "untouched"
// figure.
func RAM() (total, free uint64, ok bool) {
	total = memory.TotalMemory()
	free = memory.FreeMemory()
	if total == 0 {
		// The OS gave us nothing usable. Say so; do not invent 16e9.
		return 0, 0, false
	}
	if free > total {
		free = total
	}
	return total, free, true
}

// Interfaces enumerates this machine's real network interfaces with their medium
// and link speed, as far as the OS can be honestly interrogated.
//
// Per-OS reality (see the lane report for what was verified on real hardware):
//   - Windows: real. GetAdaptersAddresses + GetIfEntry2 give interface type,
//     NDIS physical medium, negotiated link speed and MTU.
//   - Linux: real. /sys/class/net/<if>/{speed,mtu} plus driver/uevent inspection.
//   - Everything else: honest empty list, exactly as daemon/audio.EnumerateEndpoints
//     and daemon/system/cpuload_other.go do for their unsupported platforms.
func Interfaces() ([]Interface, error) { return osInterfaces() }

// InterfaceToward returns the local Interface the OS would actually use to reach
// peer (a bare IP or "host:port"), or ok=false if that cannot be determined.
//
// It works by asking the ROUTING TABLE for the source address the kernel would
// pick for that destination, then matching that address back to an enumerated
// interface. This is deliberately not "pick the best-looking NIC": on a
// multi-homed box (LAN + Wi-Fi + Hyper-V switch + Tailscale) only the routing
// table knows which interface actually carries traffic to a given peer, and it
// is the same mechanism daemon/dataplane/advertise.go uses to decide which
// address to advertise. Consistent by construction.
func InterfaceToward(peer string) (Interface, bool) {
	src := sourceAddrToward(peer)
	if src == nil {
		return Interface{}, false
	}
	ifaces, err := Interfaces()
	if err != nil {
		return Interface{}, false
	}
	for _, in := range ifaces {
		for _, a := range in.Addrs {
			if a.Equal(src) {
				return in, true
			}
		}
	}
	return Interface{}, false
}

// LinkFor builds the contract.Link describing this node's link to peer, given a
// separately-measured round-trip time (see daemon/mesh's real libp2p ping — this
// package does not measure RTT itself because a meaningful RTT requires a live
// session with the peer, which the mesh already has).
//
// ok is false when the medium could not be honestly classified. A caller MUST
// then omit the link rather than append a zero-valued one: contract.Medium's zero
// value is MediumWiFi, so appending an unclassified Link silently tells the
// scheduler "this peer is on WiFi". Returning ok=false is how this package
// refuses to guess.
//
// A DOWN interface is also refused. In practice the routing table will not hand
// back a down interface's source address anyway, so this is belt-and-braces
// rather than the primary defence — but "classified" and "usable" are different
// questions and this function answers the second one.
func LinkFor(peer contract.PeerID, peerAddr string, rttMs float64) (contract.Link, bool) {
	in, found := InterfaceToward(peerAddr)
	if !found || !in.MediumKnown || !in.Up {
		return contract.Link{}, false
	}
	return contract.Link{
		Peer:   peer,
		Medium: in.Medium,
		Mbps:   in.Mbps,
		RTTms:  rttMs,
		MTU:    in.MTU,
	}, true
}

// sourceAddrToward asks the routing table which local source address would be
// used to reach dst, without sending a packet (connect(2) on a UDP socket only
// installs the 4-tuple). dst may be a bare IP or "host:port".
func sourceAddrToward(dst string) net.IP {
	if dst == "" {
		return nil
	}
	host := dst
	if h, _, err := net.SplitHostPort(dst); err == nil {
		host = h
	}
	if net.ParseIP(host) == nil {
		return nil
	}
	c, err := net.Dial("udp", net.JoinHostPort(host, "9"))
	if err != nil {
		return nil
	}
	defer c.Close()
	ua, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok || ua.IP == nil || ua.IP.IsUnspecified() {
		return nil
	}
	return ua.IP
}
