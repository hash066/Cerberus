//go:build !windows && !linux

package system

// readCPUTimes has no cgo-free host CPU-time counter wired on this platform
// (e.g. macOS needs host_statistics via cgo, which this build avoids). It
// reports ok=false so the sampler advertises 0 utilization — an honest
// "unknown", not a fabricated reading (maturity honesty, ARCHITECTURE §8): the
// node still participates in placement on cores/VRAM/thermal, just without a
// live load signal until a real sampler is wired here.
func readCPUTimes() (idle, total uint64, ok bool) { return 0, 0, false }
