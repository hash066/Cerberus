// VRAM probing — the honest answer to "how much GPU memory does this node have?"
//
// This is the telemetry input the scheduler ranks nodes on
// (daemon/scheduler.DefaultCostModel filters on Memory.VRAMFree and scores by it)
// and the number Lane L needs to size a llama.cpp `--tensor-split`. A WRONG number
// here is worse than no number: it silently sends a model to a node that cannot
// hold it and OOMs the GPU. So this file obeys one rule above all:
//
//	NEVER FABRICATE. A device we cannot measure is reported as unknown (zero), and
//	the Source string always names what actually produced the number — exactly the
//	discipline daemon/gpu's `backend:` string already uses for dispatch.
//
// There is deliberately no "estimate", no "typical for this class of GPU", and no
// heuristic fallback. Unknown is a first-class, useful answer: it makes the node
// infeasible for VRAM-gated placement, which is the correct outcome for a node
// whose VRAM we cannot see.
//
// # Why a subprocess (nvidia-smi) and not NVML/DXGI/Vulkan
//
// The shipped binaries are built CGO_ENABLED=0 across a 6-way cross-compile matrix
// (build/release.sh: {linux,darwin,windows} x {amd64,arm64}). That is a hard
// constraint: any probe must be pure Go. It rules out NVML and Metal (C APIs), and
// it makes vendor SDKs a non-starter. What remains is (a) OS interfaces reachable
// via syscall, (b) sysfs, and (c) subprocesses. See vramprobe_windows.go and
// vramprobe_linux.go for the per-platform sources, and docs/vram.md for the
// measured evidence behind each choice — in particular why DXGI was evaluated on a
// real RTX 3050 and REJECTED (it reports a per-process D3D budget and, for an
// integrated GPU, shared system RAM — a different quantity that merely looks like
// free VRAM).
package gpu

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// SourceUnknown is the Source value when no probe could measure this machine's
// GPU memory. Total/Free are zero and MUST be treated as "no information", never
// as "this node has no VRAM available" in a way that fabricates a measurement.
const SourceUnknown = "unknown"

// Device is one physical GPU's memory, as measured by exactly one Source.
// TotalBytes/FreeBytes are zero only when genuinely unknown — a probe that cannot
// read a field drops the device rather than guessing it.
type Device struct {
	Index int    // the probing source's own device ordinal (e.g. nvidia-smi's index)
	Name  string // vendor's device name, verbatim from the source
	// TotalBytes is the device's total memory. FreeBytes is what is currently
	// allocatable on it — as reported by the source, NOT computed as total-used
	// (drivers reserve memory that appears in neither).
	TotalBytes uint64
	FreeBytes  uint64
}

// VRAM is a node's GPU-memory snapshot: every device a source could see, plus the
// name of the source that saw them and when.
type VRAM struct {
	// Devices is empty when Source == SourceUnknown.
	Devices []Device
	// Source names the probe that produced Devices ("nvidia-smi", "amdgpu-sysfs",
	// or SourceUnknown). It is the honesty mechanism: telemetry consumers can tell
	// a measurement from an absence of one.
	Source string
	// AsOf is when the measurement was taken, so a cached snapshot's staleness is
	// always visible to its consumer.
	AsOf time.Time
	// Err records why probing failed, when Source == SourceUnknown. Diagnostic
	// only — an unknown VRAM is a normal state (no GPU, no driver), not an error
	// the daemon should act on.
	Err error
}

// Known reports whether any device was actually measured.
func (v VRAM) Known() bool { return v.Source != SourceUnknown && v.Source != "" && len(v.Devices) > 0 }

// Total and Free reduce the per-device view to the scalars contract.Memory holds.
//
// The reduction is the SINGLE BEST DEVICE (most free), deliberately NOT a sum
// across devices. Summing would imply a pool that does not physically exist: a
// task placed on this node runs on one device and can only use that device's
// memory. Reporting a laptop's 4 GiB dGPU + 128 MiB iGPU as "4.1 GiB free" would
// be exactly the plausible-looking lie this package exists to prevent. Consumers
// that can split across devices (llama.cpp --tensor-split) must read Devices.
//
// Both return 0 when unknown.
func (v VRAM) Total() uint64 {
	d, ok := v.best()
	if !ok {
		return 0
	}
	return d.TotalBytes
}
func (v VRAM) Free() uint64 {
	d, ok := v.best()
	if !ok {
		return 0
	}
	return d.FreeBytes
}

// best returns the device with the most free memory.
func (v VRAM) best() (Device, bool) {
	if !v.Known() {
		return Device{}, false
	}
	best := v.Devices[0]
	for _, d := range v.Devices[1:] {
		if d.FreeBytes > best.FreeBytes {
			best = d
		}
	}
	return best, true
}

// String renders the snapshot for logs/CLI — always naming the source, and saying
// "unknown" in as many words when that is the truth.
func (v VRAM) String() string {
	if !v.Known() {
		return "vram: unknown (no probe could measure this machine's GPU memory)"
	}
	var b strings.Builder
	b.WriteString("vram: ")
	for i, d := range v.Devices {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(d.Name)
		b.WriteString(" ")
		b.WriteString(mib(d.FreeBytes))
		b.WriteString(" free / ")
		b.WriteString(mib(d.TotalBytes))
		b.WriteString(" total")
	}
	b.WriteString(" (source: ")
	b.WriteString(v.Source)
	b.WriteString(")")
	return b.String()
}

func mib(b uint64) string {
	const mi = 1024 * 1024
	v := b / mi
	// small hand-rolled itoa keeps this dependency-free
	if v == 0 && b > 0 {
		return "<1 MiB"
	}
	digits := []byte{}
	if v == 0 {
		digits = []byte{'0'}
	}
	for v > 0 {
		digits = append([]byte{byte('0' + v%10)}, digits...)
		v /= 10
	}
	return string(digits) + " MiB"
}

// source is one way of measuring GPU memory. name is what lands in VRAM.Source,
// so it must describe the mechanism truthfully.
//
// The set of sources is per-platform (vramprobe_windows.go / _linux.go / _other.go)
// and is a plain slice on purpose: it is the extension seam. A source that needs
// another lane's code — e.g. parsing an installed llama.cpp pack's device listing,
// which is the only honest way to read Apple Silicon's unified-memory budget from a
// CGO_ENABLED=0 binary — can be appended without touching this file.
type source struct {
	name  string
	probe func() ([]Device, error)
}

// Probe measures GPU memory now, trying each platform source in order and taking
// the first that returns at least one device. It is synchronous and may spawn a
// subprocess (~100-130ms for nvidia-smi); telemetry callers want Monitor instead.
//
// It never returns an error: an unmeasurable machine is a normal state, reported
// as VRAM{Source: SourceUnknown} with the last failure in Err for diagnostics.
func Probe() VRAM {
	now := time.Now()
	var firstErr error
	for _, s := range sources() {
		devs, err := s.probe()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if len(devs) == 0 {
			continue
		}
		sort.SliceStable(devs, func(i, j int) bool { return devs[i].Index < devs[j].Index })
		return VRAM{Devices: devs, Source: s.name, AsOf: now}
	}
	return VRAM{Source: SourceUnknown, AsOf: now, Err: firstErr}
}

// DefaultTTL is how long a Monitor serves a cached snapshot before refreshing.
// Telemetry samples at 2 Hz (daemon/system composes telemetry.Config{Hz: 2}); an
// uncached ~130ms nvidia-smi on every 500ms tick would burn ~26% of a core, so the
// cache is load-bearing, not an optimization.
const DefaultTTL = 5 * time.Second

// Monitor caches Probe behind a TTL so a high-frequency sampler (telemetry) never
// pays for a subprocess and never blocks on one.
//
// Refresh policy is serve-stale-while-revalidating: once warm, Get ALWAYS returns
// immediately from cache; when the entry ages past TTL exactly one background
// refresh is started and callers keep getting the previous snapshot (whose AsOf
// tells them how old it is) until it lands. Only the very first Get on a cold
// Monitor blocks. This keeps a slow or wedged nvidia-smi from ever stalling the
// telemetry tick.
type Monitor struct {
	// TTL is the staleness bound; zero means DefaultTTL.
	TTL time.Duration
	// probe is the measurement function; nil means Probe. Injectable for tests.
	probe func() VRAM

	mu         sync.Mutex
	snap       VRAM
	warm       bool
	refreshing bool
}

// NewMonitor builds a Monitor with the default TTL.
func NewMonitor() *Monitor { return &Monitor{TTL: DefaultTTL} }

func (m *Monitor) ttl() time.Duration {
	if m.TTL <= 0 {
		return DefaultTTL
	}
	return m.TTL
}

func (m *Monitor) probeFn() func() VRAM {
	if m.probe == nil {
		return Probe
	}
	return m.probe
}

// Get returns the most recent snapshot, refreshing in the background when stale.
// The returned VRAM's AsOf is the honest measurement time — it may be older than
// TTL if a refresh is in flight or the probe is slow.
func (m *Monitor) Get() VRAM {
	m.mu.Lock()
	if !m.warm {
		// Cold: this one caller pays for the first measurement so the daemon's very
		// first telemetry sample carries a real number rather than a zero that would
		// be indistinguishable from "no GPU".
		m.warm = true
		m.mu.Unlock()
		v := m.probeFn()()
		m.mu.Lock()
		m.snap = v
		m.mu.Unlock()
		return v
	}
	snap := m.snap
	if time.Since(snap.AsOf) > m.ttl() && !m.refreshing {
		m.refreshing = true
		go func() {
			v := m.probeFn()()
			m.mu.Lock()
			m.snap = v
			m.refreshing = false
			m.mu.Unlock()
		}()
	}
	m.mu.Unlock()
	return snap
}

// defaultMonitor backs the package-level VRAMSnapshot.
var defaultMonitor = NewMonitor()

// VRAMSnapshot is the daemon's entry point: a cached, never-blocking (after the
// first call) VRAM reading suitable for a 2 Hz telemetry sampler.
//
// This is the function daemon/system/system.go's localTelemetry should call to
// replace its hardcoded VRAM literals — see docs/vram.md for the exact diff.
func VRAMSnapshot() VRAM { return defaultMonitor.Get() }
