//go:build !windows && !linux

package hostinfo

import "net"

// iface_other.go is the honest fallback for platforms with no real link
// classification wired yet — today that means darwin and the BSDs.
//
// It enumerates interfaces and addresses (net.Interfaces is portable and real,
// so InterfaceToward still resolves which interface reaches a peer), but it
// reports NO medium and NO link speed, because it genuinely does not know them.
// MediumKnown stays false, so LinkFor returns ok=false and callers omit the link
// rather than emitting one that would read as MediumWiFi (contract.Medium's zero
// value). Same posture as daemon/system/cpuload_other.go's honest 0.
//
// WHY DARWIN IS NOT DONE, stated plainly rather than half-faked: macOS exposes
// link medium via SIOCGIFMEDIA (an ioctl whose IFM_* decoding is non-trivial and
// unverifiable without a Mac to test on) or via SystemConfiguration/CoreFoundation
// (cgo). Thunderbolt Bridge on macOS is a genuinely interesting case — it is the
// one consumer platform where Thunderbolt networking is common — but writing
// unverified ioctl decoding and presenting it as working detection is exactly the
// kind of thing CLAUDE.md's maturity-honesty rule forbids. An empty, honest
// result is better than a plausible, untested one. See the lane report.
func osInterfaces() ([]Interface, error) {
	sysIfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]Interface, 0, len(sysIfaces))
	for _, si := range sysIfaces {
		in := Interface{
			Name:     si.Name,
			MTU:      uint32(si.MTU),
			Loopback: si.Flags&net.FlagLoopback != 0,
			// IFF_UP is the only portable signal here. It is weaker than the
			// Windows/Linux paths' operational state (it is administrative, so it
			// stays true for an unplugged NIC), but it is a real OS fact and
			// strictly better than claiming every interface is up. Medium stays
			// unknown regardless, so LinkFor refuses these anyway.
			Up: si.Flags&net.FlagUp != 0,
			// Medium deliberately left unknown, Mbps left 0.
		}
		if addrs, aerr := si.Addrs(); aerr == nil {
			for _, a := range addrs {
				if ipnet, ok := a.(*net.IPNet); ok {
					in.Addrs = append(in.Addrs, ipnet.IP)
				}
			}
		}
		out = append(out, in)
	}
	return out, nil
}
