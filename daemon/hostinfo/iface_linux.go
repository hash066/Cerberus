package hostinfo

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	contract "github.com/hash066/cerberus/contract/go"
)

// iface_linux.go is the real Linux enumeration, read from sysfs — no cgo, no
// netlink dependency, and nothing that needs privileges.
//
//	/sys/class/net/<if>/speed            negotiated link speed in Mbit/s
//	/sys/class/net/<if>/mtu              MTU
//	/sys/class/net/<if>/wireless/        present iff the interface is Wi-Fi
//	/sys/class/net/<if>/uevent           DEVTYPE=wlan for Wi-Fi
//	/sys/class/net/<if>/device/driver    symlink; basename is the driver name
//	                                     ("thunderbolt-net" for a TB/USB4 bridge)
//	/sys/class/net/<if>/device           ABSENT for virtual devices (veth, docker0,
//	                                     bridges, tun/tap) — this is how a virtual
//	                                     interface is told apart from a real NIC,
//	                                     the same distinction NDIS_PHYSICAL_MEDIUM
//	                                     provides on Windows.
//
// NOT VERIFIED ON REAL LINUX HARDWARE: the dev rig is Windows. This code is
// written against documented sysfs semantics and compiles, but the lane report
// lists it as unverified rather than claiming a tested platform.

const sysClassNet = "/sys/class/net"

func osInterfaces() ([]Interface, error) {
	sysIfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []Interface
	for _, si := range sysIfaces {
		in := Interface{
			Name:     si.Name,
			MTU:      uint32(si.MTU),
			Loopback: si.Flags&net.FlagLoopback != 0,
		}
		if addrs, aerr := si.Addrs(); aerr == nil {
			for _, a := range addrs {
				if ipnet, ok := a.(*net.IPNet); ok {
					in.Addrs = append(in.Addrs, ipnet.IP)
				}
			}
		}
		if in.Loopback {
			out = append(out, in)
			continue
		}
		base := filepath.Join(sysClassNet, si.Name)
		in.Mbps = readSpeedMbps(base)
		// No backing device => virtual (veth, bridge, docker0, tun/tap, wg).
		if _, derr := os.Stat(filepath.Join(base, "device")); derr != nil {
			in.Virtual = true
			out = append(out, in) // medium stays unknown: see honesty rule 3.
			continue
		}
		in.Medium, in.MediumKnown = classifyLinux(base)
		out = append(out, in)
	}
	return out, nil
}

func classifyLinux(base string) (contract.Medium, bool) {
	// Thunderbolt/USB4 first: strictly more specific than "it's an Ethernet-like
	// device". The driver name is a real kernel fact here, not a description
	// string heuristic — this is a stronger signal than the Windows path has.
	if drv := driverName(base); drv != "" {
		switch {
		case strings.Contains(drv, "thunderbolt"):
			return contract.MediumThunderbolt, true
		}
	}
	if isWirelessLinux(base) {
		return contract.MediumWiFi, true
	}
	// A real device that is not wireless and not Thunderbolt: wired Ethernet.
	// Guarded by the caller having already established a backing device exists.
	if _, err := os.Stat(filepath.Join(base, "device")); err == nil {
		return contract.MediumEth, true
	}
	return 0, false
}

// driverName returns the basename of /sys/class/net/<if>/device/driver.
func driverName(base string) string {
	dst, err := os.Readlink(filepath.Join(base, "device", "driver"))
	if err != nil {
		return ""
	}
	return strings.ToLower(filepath.Base(dst))
}

// isWirelessLinux reports whether the interface is Wi-Fi. The wireless/ directory
// is the classic signal; DEVTYPE=wlan in uevent is the modern one. Either is
// conclusive.
func isWirelessLinux(base string) bool {
	if _, err := os.Stat(filepath.Join(base, "wireless")); err == nil {
		return true
	}
	b, err := os.ReadFile(filepath.Join(base, "uevent"))
	if err != nil {
		return false
	}
	return strings.Contains(string(b), "DEVTYPE=wlan")
}

// readSpeedMbps reads the negotiated link speed. sysfs reports -1 (and EINVAL on
// some drivers) when the link is down or the speed is unknown; both mean 0 here.
func readSpeedMbps(base string) float64 {
	b, err := os.ReadFile(filepath.Join(base, "speed"))
	if err != nil {
		return 0
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || v <= 0 {
		return 0
	}
	return float64(v)
}
