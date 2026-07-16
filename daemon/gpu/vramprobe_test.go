package gpu

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// The RTX 3050 Laptop GPU row this lane's test box actually emits, captured
// verbatim from `nvidia-smi --query-gpu=index,name,memory.total,memory.free
// --format=csv,noheader,nounits`.
const realRTX3050Row = "0, NVIDIA GeForce RTX 3050 Laptop GPU, 4096, 3861\n"

func TestParseNvidiaSMIRealRTX3050(t *testing.T) {
	devs, err := parseNvidiaSMI(realRTX3050Row)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(devs) != 1 {
		t.Fatalf("want 1 device, got %d", len(devs))
	}
	d := devs[0]
	if d.Index != 0 {
		t.Errorf("Index = %d, want 0", d.Index)
	}
	if d.Name != "NVIDIA GeForce RTX 3050 Laptop GPU" {
		t.Errorf("Name = %q", d.Name)
	}
	// The whole point: MiB -> bytes, exactly, with no rounding or estimation.
	if want := uint64(4096) * 1024 * 1024; d.TotalBytes != want {
		t.Errorf("TotalBytes = %d, want %d", d.TotalBytes, want)
	}
	if want := uint64(3861) * 1024 * 1024; d.FreeBytes != want {
		t.Errorf("FreeBytes = %d, want %d", d.FreeBytes, want)
	}
}

func TestParseNvidiaSMIMultiGPU(t *testing.T) {
	out := "0, NVIDIA GeForce RTX 3050 Laptop GPU, 4096, 3861\n" +
		"1, NVIDIA RTX A6000, 49140, 48000\n"
	devs, err := parseNvidiaSMI(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(devs) != 2 {
		t.Fatalf("want 2 devices, got %d", len(devs))
	}
	if devs[1].Name != "NVIDIA RTX A6000" || devs[1].Index != 1 {
		t.Errorf("second device = %+v", devs[1])
	}
}

// The honesty contract: rows nvidia-smi could not answer must NOT become numbers.
func TestParseNvidiaSMIRefusesUnparsableRows(t *testing.T) {
	for name, out := range map[string]string{
		"N/A memory":     "0, NVIDIA A100-SXM4-40GB, [N/A], [N/A]\n",
		"not supported":  "0, Some vGPU, [Not Supported], [Not Supported]\n",
		"zero total":     "0, Broken GPU, 0, 0\n",
		"truncated row":  "0, NVIDIA GeForce RTX 3050 Laptop GPU, 4096\n",
		"empty output":   "",
		"driver garbage": "Failed to initialize NVML: Driver/library version mismatch\n",
	} {
		t.Run(name, func(t *testing.T) {
			devs, err := parseNvidiaSMI(out)
			if err == nil {
				t.Fatalf("want error, got devices %+v", devs)
			}
			if len(devs) != 0 {
				t.Fatalf("want no devices, got %+v", devs)
			}
		})
	}
}

// A good row alongside a bad one keeps the good one and drops the bad one — it
// must never be defaulted into existence.
func TestParseNvidiaSMIMixedGoodAndBad(t *testing.T) {
	out := "0, NVIDIA GeForce RTX 3050 Laptop GPU, 4096, 3861\n" +
		"1, NVIDIA vGPU, [N/A], [N/A]\n"
	devs, err := parseNvidiaSMI(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(devs) != 1 || devs[0].Index != 0 {
		t.Fatalf("want only device 0, got %+v", devs)
	}
}

func TestVRAMUnknownIsHonest(t *testing.T) {
	v := VRAM{Source: SourceUnknown}
	if v.Known() {
		t.Error("unknown snapshot must not report Known")
	}
	if v.Total() != 0 || v.Free() != 0 {
		t.Errorf("unknown must be zero, got total=%d free=%d", v.Total(), v.Free())
	}
	if !strings.Contains(v.String(), "unknown") {
		t.Errorf("String() must say unknown, got %q", v.String())
	}
}

// The scalar reduction must be the best SINGLE device, never a sum: a task runs on
// one GPU and can only use that GPU's memory.
func TestVRAMScalarReductionIsBestDeviceNotSum(t *testing.T) {
	v := VRAM{
		Source: nvidiaSMISourceName,
		Devices: []Device{
			{Index: 0, Name: "iGPU", TotalBytes: 128 << 20, FreeBytes: 100 << 20},
			{Index: 1, Name: "RTX 3050", TotalBytes: 4096 << 20, FreeBytes: 3861 << 20},
		},
	}
	if got, want := v.Free(), uint64(3861)<<20; got != want {
		t.Errorf("Free() = %d, want %d (best device, not sum)", got, want)
	}
	if got, want := v.Total(), uint64(4096)<<20; got != want {
		t.Errorf("Total() = %d, want %d (best device's total)", got, want)
	}
}

func TestProbeNvidiaSMIAbsentIsCleanUnknown(t *testing.T) {
	orig := lookPath
	t.Cleanup(func() { lookPath = orig })
	lookPath = func(string) (string, error) { return "", exec.ErrNotFound }

	if _, err := probeNvidiaSMI(); err == nil {
		t.Fatal("want error when nvidia-smi is absent")
	}
}

func TestProbeNvidiaSMIWiring(t *testing.T) {
	origLook, origRun := lookPath, runCommand
	t.Cleanup(func() { lookPath, runCommand = origLook, origRun })

	lookPath = func(string) (string, error) { return "/usr/bin/nvidia-smi", nil }
	var gotArgs []string
	runCommand = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(realRTX3050Row), nil
	}
	devs, err := probeNvidiaSMI()
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(devs) != 1 || devs[0].TotalBytes != 4096<<20 {
		t.Fatalf("devices = %+v", devs)
	}
	// free must be queried, never derived from total-used.
	joined := strings.Join(gotArgs, " ")
	if !strings.Contains(joined, "memory.free") {
		t.Errorf("must query memory.free directly, args = %q", joined)
	}
	if !strings.Contains(joined, "nounits") {
		t.Errorf("parser assumes nounits, args = %q", joined)
	}
}

func TestProbeNvidiaSMICommandFailure(t *testing.T) {
	origLook, origRun := lookPath, runCommand
	t.Cleanup(func() { lookPath, runCommand = origLook, origRun })

	lookPath = func(string) (string, error) { return "/usr/bin/nvidia-smi", nil }
	runCommand = func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("exit status 255")
	}
	if _, err := probeNvidiaSMI(); err == nil {
		t.Fatal("want error when nvidia-smi fails")
	}
}

// Monitor: cold Get blocks and measures once; subsequent Gets are served from
// cache without re-probing.
func TestMonitorCachesWithinTTL(t *testing.T) {
	var calls int
	var mu sync.Mutex
	m := &Monitor{TTL: time.Hour, probe: func() VRAM {
		mu.Lock()
		calls++
		mu.Unlock()
		return VRAM{Source: nvidiaSMISourceName, AsOf: time.Now(),
			Devices: []Device{{Name: "gpu", TotalBytes: 1 << 30, FreeBytes: 1 << 29}}}
	}}
	for i := 0; i < 10; i++ {
		if v := m.Get(); !v.Known() {
			t.Fatal("want known snapshot")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("probe called %d times, want 1 (cached within TTL)", calls)
	}
}

// The load-bearing property: once warm, Get must never block on a slow probe. A
// wedged nvidia-smi must not stall the 2 Hz telemetry tick.
func TestMonitorServesStaleWithoutBlocking(t *testing.T) {
	release := make(chan struct{})
	probed := make(chan struct{}, 8)
	var once sync.Once
	m := &Monitor{TTL: time.Millisecond, probe: func() VRAM {
		select {
		case probed <- struct{}{}:
		default:
		}
		// Only the refresh (second call) blocks; the cold call returns at once.
		var first bool
		once.Do(func() { first = true })
		if !first {
			<-release
		}
		return VRAM{Source: nvidiaSMISourceName, AsOf: time.Now(),
			Devices: []Device{{Name: "gpu", TotalBytes: 1 << 30, FreeBytes: 1 << 29}}}
	}}

	warm := m.Get() // cold: pays for the first measurement
	if !warm.Known() {
		t.Fatal("cold Get must return a real measurement")
	}
	time.Sleep(5 * time.Millisecond) // let it go stale

	done := make(chan VRAM, 1)
	go func() { done <- m.Get() }()
	select {
	case v := <-done:
		if !v.Known() {
			t.Error("stale Get must still return the previous real snapshot")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Get blocked on a stale refresh — telemetry would stall")
	}
	close(release)
}

func TestMonitorSingleFlightRefresh(t *testing.T) {
	var mu sync.Mutex
	var calls int
	m := &Monitor{TTL: time.Millisecond, probe: func() VRAM {
		mu.Lock()
		calls++
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		return VRAM{Source: nvidiaSMISourceName, AsOf: time.Now(),
			Devices: []Device{{Name: "gpu", TotalBytes: 1 << 30, FreeBytes: 1 << 29}}}
	}}
	m.Get()                          // cold -> 1 call
	time.Sleep(5 * time.Millisecond) // stale

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ { // stampede
		wg.Add(1)
		go func() { defer wg.Done(); m.Get() }()
	}
	wg.Wait()
	time.Sleep(60 * time.Millisecond) // let the refresh land

	mu.Lock()
	defer mu.Unlock()
	if calls > 2 {
		t.Errorf("probe called %d times, want <=2 (cold + one single-flight refresh)", calls)
	}
}

// Probe must never panic or error on any machine, GPU or not — unknown is a
// normal, expected state.
func TestProbeNeverErrorsOnThisMachine(t *testing.T) {
	v := Probe()
	if v.AsOf.IsZero() {
		t.Error("AsOf must always be stamped")
	}
	if v.Known() {
		for _, d := range v.Devices {
			if d.TotalBytes == 0 {
				t.Errorf("known device with zero total: %+v", d)
			}
			if d.FreeBytes > d.TotalBytes {
				t.Errorf("free > total is impossible: %+v", d)
			}
		}
		t.Logf("probed: %s", v)
	} else {
		t.Logf("no GPU measurable on this machine (source=%s, err=%v)", v.Source, v.Err)
	}
}

// Refresh must beat the TTL: after a session frees VRAM the node has to stop
// advertising the busy figure, rather than under-reporting until the TTL expires.
func TestMonitorRefreshBeatsTTL(t *testing.T) {
	var mu sync.Mutex
	free := uint64(1 << 29) // "busy"
	m := &Monitor{TTL: time.Hour, probe: func() VRAM {
		mu.Lock()
		f := free
		mu.Unlock()
		return VRAM{Source: nvidiaSMISourceName, AsOf: time.Now(),
			Devices: []Device{{Name: "gpu", TotalBytes: 1 << 30, FreeBytes: f}}}
	}}
	if got := m.Get().Free(); got != 1<<29 {
		t.Fatalf("warm-up Free() = %d, want %d", got, uint64(1<<29))
	}

	// The session ends: VRAM is actually free again.
	mu.Lock()
	free = 1 << 30
	mu.Unlock()

	// Without Refresh the hour-long TTL would pin the stale value.
	if got := m.Get().Free(); got != 1<<29 {
		t.Fatalf("Free() = %d — cache should still be serving the stale value here", got)
	}
	m.Refresh()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if m.Get().Free() == 1<<30 {
			return // converged despite the TTL
		}
		if time.Now().After(deadline) {
			t.Fatal("Refresh did not re-measure: Get still serves the pre-teardown value")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Refresh must never make a caller block, even on a wedged probe.
func TestMonitorRefreshNeverBlocksGet(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	m := &Monitor{TTL: time.Hour, probe: func() VRAM {
		once.Do(func() {}) // first call (warm-up) returns immediately
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		return VRAM{Source: nvidiaSMISourceName, AsOf: time.Now(),
			Devices: []Device{{Name: "gpu", TotalBytes: 1 << 30, FreeBytes: 1 << 30}}}
	}}
	close(release) // let the synchronous warm-up through
	_ = m.Get()

	release = make(chan struct{}) // now wedge the probe
	m.Refresh()
	done := make(chan struct{})
	go func() { _ = m.Get(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Get blocked behind a wedged Refresh — the telemetry tick would stall")
	}
}

// A cold Monitor must not be faked warm: a concurrent Get would then read a zero
// snapshot, which is indistinguishable from "this machine has no GPU".
func TestMonitorRefreshColdDoesNotFabricate(t *testing.T) {
	var calls int
	var mu sync.Mutex
	m := &Monitor{TTL: time.Hour, probe: func() VRAM {
		mu.Lock()
		calls++
		mu.Unlock()
		return VRAM{Source: nvidiaSMISourceName, AsOf: time.Now(),
			Devices: []Device{{Name: "gpu", TotalBytes: 1 << 30, FreeBytes: 1 << 30}}}
	}}
	m.Refresh() // cold: must be a no-op
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 0 {
		t.Fatalf("Refresh on a cold monitor probed %d times, want 0", got)
	}
	if v := m.Get(); !v.Known() || v.Free() != 1<<30 {
		t.Fatalf("first Get after a cold Refresh must still return a real measurement, got %+v", v)
	}
}
