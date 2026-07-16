package hostinfo

import (
	"fmt"
	"net"
	"strings"
	"unsafe"

	contract "github.com/hash066/cerberus/contract/go"
	"golang.org/x/sys/windows"
)

// iface_windows.go is the REAL Windows interface enumeration: GetAdaptersAddresses
// for the address/type/speed/MTU picture, plus GetIfEntry2 for the NDIS PHYSICAL
// medium — which is the field that distinguishes a real NIC from a virtual switch
// pretending to be one.
//
// Why both calls: GetAdaptersAddresses gives IfType, which separates Wi-Fi
// (IF_TYPE_IEEE80211) from Ethernet (IF_TYPE_ETHERNET_CSMACD) — but a Hyper-V/WSL
// virtual switch ALSO reports IF_TYPE_ETHERNET_CSMACD, at 10 Gbps, and is
// unreachable from any other machine. Only NDIS_PHYSICAL_MEDIUM tells them apart:
// a real Ethernet NIC reports NdisPhysicalMedium802_3, the virtual switch reports
// NdisPhysicalMediumUnspecified. Verified on the dev rig, where Get-NetAdapter
// shows exactly that split (Realtek GbE => "802.3"; vEthernet (WSL) => 10 Gbps,
// "Unspecified").

// NDIS_PHYSICAL_MEDIUM values from ntddndis.h. x/sys/windows does not export
// these, so they are declared here with their upstream names.
const (
	ndisPhysicalMediumUnspecified  = 0 // virtual/software adapter — NOT a real link
	ndisPhysicalMediumWirelessLan  = 1
	ndisPhysicalMedium1394         = 7
	ndisPhysicalMediumNative80211  = 9
	ndisPhysicalMediumBluetooth    = 10
	ndisPhysicalMediumInfiniband   = 11
	ndisPhysicalMedium8023         = 14 // real wired Ethernet
	ndisPhysicalMediumOther        = 19
	ndisPhysicalMediumUnsupported_ = 0xffffffff
)

// osInterfaces enumerates real adapters and classifies each one's medium.
func osInterfaces() ([]Interface, error) {
	rows, err := adapterAddresses()
	if err != nil {
		return nil, err
	}
	var out []Interface
	for _, a := range rows {
		in := Interface{
			Name: windows.UTF16PtrToString(a.FriendlyName),
			MTU:  a.Mtu,
			// IfOperStatus. A classified medium says what a link IS, not whether
			// it exists: the dev rig's unplugged Realtek GbE reports a real 802.3
			// physical medium and would otherwise look like a usable Ethernet link.
			Up: a.OperStatus == windows.IfOperStatusUp,
		}
		// ReceiveLinkSpeed is in bits/sec. Some virtual/disconnected adapters
		// report the sentinel ^uint64(0); treat that as unknown rather than as an
		// absurd speed.
		if a.ReceiveLinkSpeed != ^uint64(0) && a.ReceiveLinkSpeed > 0 {
			in.Mbps = float64(a.ReceiveLinkSpeed) / 1e6
		}
		for _, ip := range unicastIPs(a) {
			in.Addrs = append(in.Addrs, ip)
		}
		if a.IfType == windows.IF_TYPE_SOFTWARE_LOOPBACK {
			in.Loopback = true
			out = append(out, in)
			continue
		}
		desc := windows.UTF16PtrToString(a.Description)
		phys, physOK := physicalMedium(a.Luid)
		in.Virtual = physOK && phys == ndisPhysicalMediumUnspecified
		in.Medium, in.MediumKnown = classify(a.IfType, phys, physOK, desc, in.Virtual)
		out = append(out, in)
	}
	return out, nil
}

// classify maps (IfType, NDIS physical medium, adapter description) onto a
// contract.Medium, returning known=false whenever the OS did not give us enough
// to be sure. Every false here is deliberate: contract.Medium's zero value is
// MediumWiFi, so a wrong "true" is a silent lie to the scheduler, while a false
// merely omits the link.
func classify(ifType, phys uint32, physOK bool, desc string, virtual bool) (contract.Medium, bool) {
	// A virtual switch/tunnel has no physical medium. It may look like fast
	// Ethernet (the WSL vEthernet adapter reports 10 Gbps); classifying it would
	// tell the scheduler this node has a 10 Gbps Ethernet link to a peer it
	// cannot reach at all.
	if virtual || ifType == windows.IF_TYPE_TUNNEL {
		return 0, false
	}

	// Thunderbolt / USB4 networking. HONESTY NOTE: Windows exposes no distinct
	// IfType or NDIS medium for these — a Thunderbolt bridge presents as ordinary
	// IF_TYPE_ETHERNET_CSMACD / NdisPhysicalMedium802_3. The adapter DESCRIPTION
	// is the only discriminator available without vendor APIs, so this is a
	// string heuristic, and it is checked before the generic Ethernet mapping
	// because a match is strictly more specific. It has NOT been verified against
	// real Thunderbolt hardware (the dev rig has none) — see the lane report. A
	// miss degrades to MediumEth, which is a safe, honest fallback: a Thunderbolt
	// bridge really is an Ethernet-like link, just a faster one.
	if isThunderbolt(desc) {
		return contract.MediumThunderbolt, true
	}

	switch ifType {
	case windows.IF_TYPE_IEEE80211:
		return contract.MediumWiFi, true
	case windows.IF_TYPE_ETHERNET_CSMACD:
		if !physOK {
			// IfType says Ethernet but we could not confirm a physical medium.
			// Most likely still Ethernet, but "most likely" is how a virtual
			// switch gets misreported as a 10 Gbps link. Refuse.
			return 0, false
		}
		switch phys {
		case ndisPhysicalMedium8023:
			return contract.MediumEth, true
		case ndisPhysicalMediumNative80211, ndisPhysicalMediumWirelessLan:
			// Some drivers report Wi-Fi under an Ethernet IfType.
			return contract.MediumWiFi, true
		default:
			// Bluetooth PAN, 1394, Infiniband, "Other", etc. The contract's four
			// mediums cannot express these, and mapping Bluetooth onto MediumEth
			// would be a lie the scheduler would act on. Omit.
			// (NB: Infiniband is NOT mapped to MediumRDMATB — that constant stays
			// unused, and RDMA stays Frontier. See hostinfo.go honesty rule 2.)
			return 0, false
		}
	default:
		return 0, false
	}
}

// isThunderbolt matches the adapter descriptions Windows gives Thunderbolt/USB4
// networking. Windows names these "Thunderbolt(TM) Bridge" / "USB4 Network
// Adapter"; matching is case-insensitive and substring-based because vendors
// prefix their own names.
func isThunderbolt(desc string) bool {
	d := strings.ToLower(desc)
	return strings.Contains(d, "thunderbolt") || strings.Contains(d, "usb4")
}

// MIB_IF_ROW2 layout constants, taken from the Windows SDK rather than from Go's
// struct — because on 386 the two DISAGREE and the disagreement is a memory-
// corrupting bug.
//
// MIB_IF_ROW2 contains no pointers, so its C layout is IDENTICAL on 32- and
// 64-bit Windows: the SDK's default pack(8) aligns each ULONGLONG to 8, giving
// TransmitLinkSpeed at offset 1192 and sizeof == 1352 on both.
//
// Go's 386 ABI, however, aligns uint64 to FOUR bytes. So on 386
// golang.org/x/sys/windows.MibIfRow2 is laid out with TransmitLinkSpeed at 1188
// and sizeof == 1348 — four bytes SHORTER than the struct the kernel writes.
// Passing it to GetIfEntry2Ex lets iphlpapi write 1352 bytes into 1348 bytes of
// Go memory, which reliably crashes with an access violation (0xc0000005), and
// silently misreads every field past offset 1188 even when it does not. Verified
// on this rig: `go test` (the toolchain here is GOHOSTARCH=386) crashed in
// exactly this call, while GOARCH=amd64 passed.
//
// This is an upstream x/sys bug, not something this package can fix, so we route
// around it: hand the API a raw buffer of the TRUE C size and read the one field
// we need at its documented C offset. PhysicalMediumType lives at offset 1140,
// which is in the prefix where both ABIs agree (confirmed on both arches), so the
// read is correct on 386 and amd64 alike.
const (
	// sizeofMibIfRow2C is sizeof(MIB_IF_ROW2) as the Windows SDK defines it.
	sizeofMibIfRow2C = 1352
	// offsetInterfaceLuid is MIB_IF_ROW2.InterfaceLuid (the input field).
	offsetInterfaceLuid = 0
	// offsetPhysicalMediumType is MIB_IF_ROW2.PhysicalMediumType.
	offsetPhysicalMediumType = 1140
)

// physicalMedium reads NDIS_PHYSICAL_MEDIUM for an adapter LUID via GetIfEntry2Ex.
// ok=false means the call failed or the driver reported nothing usable.
//
// It deliberately does NOT use windows.MibIfRow2's fields — see the constants
// above for why that struct is unsafe on 386.
func physicalMedium(luid uint64) (uint32, bool) {
	// Size the buffer to the larger of the true C size and whatever Go thinks the
	// struct is, so the kernel can never write past our allocation on any arch,
	// even if x/sys's definition changes.
	n := sizeofMibIfRow2C
	if goSize := int(unsafe.Sizeof(windows.MibIfRow2{})); goSize > n {
		n = goSize
	}
	// A []uint64 backing array guarantees 8-byte alignment, which the API's
	// ULONGLONG fields require.
	buf := make([]uint64, (n+7)/8)
	raw := unsafe.Slice((*byte)(unsafe.Pointer(&buf[0])), n)

	// Input: the LUID identifying which interface to look up.
	*(*uint64)(unsafe.Pointer(&raw[offsetInterfaceLuid])) = luid

	//nolint:gosec // deliberate: the buffer is >= the kernel's true struct size.
	row := (*windows.MibIfRow2)(unsafe.Pointer(&raw[0]))
	if err := windows.GetIfEntry2Ex(windows.MibIfEntryNormalWithoutStatistics, row); err != nil {
		return 0, false
	}
	phys := *(*uint32)(unsafe.Pointer(&raw[offsetPhysicalMediumType]))
	if phys == ndisPhysicalMediumUnsupported_ {
		return 0, false
	}
	return phys, true
}

// unicastIPs extracts the unicast IPs bound to an adapter. SocketAddress.IP()
// does the sockaddr decoding for us and returns nil for a family it does not
// handle, which we skip.
func unicastIPs(a *windows.IpAdapterAddresses) []net.IP {
	var out []net.IP
	for u := a.FirstUnicastAddress; u != nil; u = u.Next {
		if ip := u.Address.IP(); ip != nil {
			out = append(out, ip)
		}
	}
	return out
}

// adapterAddresses calls GetAdaptersAddresses, growing the buffer as the API
// asks. The returned pointers alias the buffer, which is kept alive by the
// returned slice referencing into it.
func adapterAddresses() ([]*windows.IpAdapterAddresses, error) {
	const flags = windows.GAA_FLAG_SKIP_ANYCAST |
		windows.GAA_FLAG_SKIP_MULTICAST |
		windows.GAA_FLAG_SKIP_DNS_SERVER
	size := uint32(15 * 1024)
	for attempt := 0; attempt < 4; attempt++ {
		buf := make([]byte, size)
		//nolint:gosec // required cast: the API writes a linked list into buf.
		first := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0]))
		err := windows.GetAdaptersAddresses(windows.AF_UNSPEC, flags, 0, first, &size)
		if err == nil {
			var out []*windows.IpAdapterAddresses
			for a := first; a != nil; a = a.Next {
				out = append(out, a)
			}
			return out, nil
		}
		if err != windows.ERROR_BUFFER_OVERFLOW {
			return nil, fmt.Errorf("hostinfo: GetAdaptersAddresses: %w", err)
		}
		// size now holds the required length; loop and retry.
	}
	return nil, fmt.Errorf("hostinfo: GetAdaptersAddresses: buffer kept growing")
}
