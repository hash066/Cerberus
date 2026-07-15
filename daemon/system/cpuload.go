package system

import "sync"

// cpuLoadSampler measures host CPU utilization as a fraction in [0,1] by
// differencing the OS's cumulative idle/total CPU-time counters between calls.
//
// It is REAL on Windows (GetSystemTimes) and Linux (/proc/stat) — the two
// platforms this daemon builds and CI-tests on today — and an honest 0
// ("unknown", a documented stub) on other platforms where no cheap, cgo-free
// counter is wired yet (see readCPUTimes per-OS files). This is deliberately
// utilization, not a load average: the scheduler wants "how busy is this box
// right now" to keep CPU-bound work off a saturated node, and utilization is
// the directly comparable signal across heterogeneous machines.
//
// The first call has no previous reading, so it returns 0 (nothing to diff yet);
// every subsequent call reflects the busy fraction over the interval since the
// previous sample. The sampler is shared process-wide (one host, one CPU), so a
// single reading is differenced against whichever caller sampled last.
type cpuLoadSampler struct {
	mu       sync.Mutex
	prevIdle uint64
	prevTot  uint64
	have     bool
}

// hostCPULoad is the process-wide host CPU utilization sampler. localTelemetry
// reads it so the FLOPS this node advertises to the placement brain reflect how
// busy the box actually is (available FLOPS = peak × (1 − utilization)).
var hostCPULoad = &cpuLoadSampler{}

// Sample returns the host CPU busy fraction in [0,1] over the interval since the
// previous call (0 when the OS counter is unavailable or on the first reading).
func (s *cpuLoadSampler) Sample() float64 {
	idle, total, ok := readCPUTimes()
	if !ok {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.have {
		s.prevIdle, s.prevTot, s.have = idle, total, true
		return 0
	}
	// Counters are monotonic cumulative ticks; unsigned wrap is not expected over
	// a daemon's lifetime, but guard against a non-increasing/negative delta.
	dIdle := int64(idle - s.prevIdle)
	dTot := int64(total - s.prevTot)
	s.prevIdle, s.prevTot = idle, total
	if dTot <= 0 || dIdle < 0 {
		return 0
	}
	busy := float64(dTot-dIdle) / float64(dTot)
	if busy < 0 {
		return 0
	}
	if busy > 1 {
		return 1
	}
	return busy
}
