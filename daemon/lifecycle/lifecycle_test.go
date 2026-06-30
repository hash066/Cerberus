package lifecycle_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/lifecycle"
)

// mockCrdt counts checkpoints so we can assert the sleep choreography sealed the
// CRDT memory doc before the node went dark.
type mockCrdt struct {
	mu          sync.Mutex
	checkpoints int
	failNext    bool
}

func (m *mockCrdt) Apply(op contract.CrdtOp) error { return nil }
func (m *mockCrdt) Merge(remote []contract.CrdtOp) ([]contract.BeliefConflict, error) {
	return nil, nil
}
func (m *mockCrdt) Snapshot(docID []byte) ([]byte, error) { return nil, nil }
func (m *mockCrdt) Checkpoint(docID []byte) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failNext {
		m.failNext = false
		return nil, errors.New("checkpoint failed")
	}
	m.checkpoints++
	return nil, nil
}
func (m *mockCrdt) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.checkpoints
}

// fakeCoordinator is the scheduler stub: it records the hand-back + standby
// promotion and the resume, so we can assert the "lid-drop" signal fired.
type fakeCoordinator struct {
	mu       sync.Mutex
	prepares []lifecycle.SleepPrepare
	resumes  []lifecycle.WakePrepare
}

func (c *fakeCoordinator) HandBackAndPromote(p lifecycle.SleepPrepare) error {
	c.mu.Lock()
	c.prepares = append(c.prepares, p)
	c.mu.Unlock()
	return nil
}
func (c *fakeCoordinator) Resume(w lifecycle.WakePrepare) error {
	c.mu.Lock()
	c.resumes = append(c.resumes, w)
	c.mu.Unlock()
	return nil
}
func (c *fakeCoordinator) prepareCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.prepares)
}
func (c *fakeCoordinator) lastPrepare() (lifecycle.SleepPrepare, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.prepares) == 0 {
		return lifecycle.SleepPrepare{}, false
	}
	return c.prepares[len(c.prepares)-1], true
}
func (c *fakeCoordinator) resumeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.resumes)
}

// recorder collects emitted lifecycle events in order, concurrency-safe.
type recorder struct {
	mu sync.Mutex
	ev []lifecycle.LifecycleEvent
}

func (r *recorder) on(ev lifecycle.LifecycleEvent) {
	r.mu.Lock()
	r.ev = append(r.ev, ev)
	r.mu.Unlock()
}
func (r *recorder) has(want lifecycle.LifecycleEvent) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.ev {
		if e == want {
			return true
		}
	}
	return false
}
func (r *recorder) count(want lifecycle.LifecycleEvent) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.ev {
		if e == want {
			n++
		}
	}
	return n
}

// waitFor polls cond until it returns true or the deadline elapses.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}

// TestPrepareSleepEmitsHandBackSignal is the core "lid-drop" assertion: on
// SLEEP_IMMINENT the monitor checkpoints, emits the event, and hands back +
// requests standby promotion via the coordinator; WAKE then recovers.
func TestPrepareSleepEmitsHandBackSignal(t *testing.T) {
	crdt := &mockCrdt{}
	coord := &fakeCoordinator{}
	rec := &recorder{}

	mon := lifecycle.NewMonitor(crdt, []byte("doc1"))
	mon.SetCoordinator(coord)
	mon.SetHeldCaps([][]byte{[]byte("cap-A"), []byte("cap-B")})
	mon.OnTransition(rec.on)

	if err := mon.PrepareSleep(); err != nil {
		t.Fatalf("PrepareSleep: %v", err)
	}

	if !rec.has(lifecycle.SleepImminent) {
		t.Error("expected SLEEP_IMMINENT event")
	}
	if crdt.count() != 1 {
		t.Errorf("expected 1 checkpoint, got %d", crdt.count())
	}
	if coord.prepareCount() != 1 {
		t.Fatalf("expected 1 hand-back/promote, got %d", coord.prepareCount())
	}
	p, _ := coord.lastPrepare()
	if string(p.DocID) != "doc1" {
		t.Errorf("hand-back carried wrong doc: %q", p.DocID)
	}
	if len(p.HeldCaps) != 2 {
		t.Errorf("expected 2 caps handed back, got %d", len(p.HeldCaps))
	}
	if mon.State().Hint != contract.SleepImminent {
		t.Errorf("expected state hint SLEEP_IMMINENT, got %v", mon.State().Hint)
	}

	// WAKE transition recovers.
	if err := mon.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !rec.has(lifecycle.Wake) {
		t.Error("expected WAKE event")
	}
	if coord.resumeCount() != 1 {
		t.Errorf("expected 1 coordinator resume, got %d", coord.resumeCount())
	}
	if mon.State().Hint == contract.SleepImminent {
		t.Error("expected sleep hint cleared after resume")
	}
}

// TestSleepImminentViaEventSource drives the same recovery path through the
// injectable event source (the simulated OS hook), proving the state machine —
// not just the manual PrepareSleep call — produces the hand-back signal.
func TestSleepImminentViaEventSource(t *testing.T) {
	crdt := &mockCrdt{}
	coord := &fakeCoordinator{}
	rec := &recorder{}
	src := lifecycle.NewChannelSource()

	mon := lifecycle.NewMonitor(crdt, []byte("doc-evt"))
	mon.SetSource(src)
	mon.SetCoordinator(coord)
	mon.OnTransition(rec.on)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mon.Start(ctx)

	// OS pre-announces imminent sleep.
	src.Push(lifecycle.PowerEvent{Kind: lifecycle.EvSleepImminent})

	waitFor(t, func() bool { return coord.prepareCount() == 1 })
	if !rec.has(lifecycle.SleepImminent) {
		t.Error("expected SLEEP_IMMINENT from event source")
	}
	if crdt.count() != 1 {
		t.Errorf("expected checkpoint, got %d", crdt.count())
	}
	p, _ := coord.lastPrepare()
	if p.Reason != lifecycle.ReasonSleepImminent {
		t.Errorf("expected ReasonSleepImminent, got %v", p.Reason)
	}

	// OS reports wake.
	src.Push(lifecycle.PowerEvent{Kind: lifecycle.EvWake})
	waitFor(t, func() bool { return coord.resumeCount() == 1 })
	if !rec.has(lifecycle.Wake) {
		t.Error("expected WAKE from event source")
	}

	mon.Stop()
}

// TestLidCloseTriggersSleep verifies a lid-close emits LID_CLOSED and is treated
// as imminent sleep (the laptop lid-drop case).
func TestLidCloseTriggersSleep(t *testing.T) {
	crdt := &mockCrdt{}
	coord := &fakeCoordinator{}
	rec := &recorder{}
	src := lifecycle.NewChannelSource()

	mon := lifecycle.NewMonitor(crdt, []byte("doc-lid"))
	mon.SetSource(src)
	mon.SetCoordinator(coord)
	mon.OnTransition(rec.on)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mon.Start(ctx)

	src.Push(lifecycle.PowerEvent{Kind: lifecycle.EvLidClosed})

	waitFor(t, func() bool { return coord.prepareCount() == 1 })
	if !rec.has(lifecycle.LidClosed) {
		t.Error("expected LID_CLOSED event")
	}
	if !rec.has(lifecycle.SleepImminent) {
		t.Error("lid close should also raise SLEEP_IMMINENT")
	}
	p, _ := coord.lastPrepare()
	if p.Reason != lifecycle.ReasonLidClosed {
		t.Errorf("expected ReasonLidClosed, got %v", p.Reason)
	}
	if mon.State().Lid != contract.LidClosed {
		t.Errorf("expected lid state CLOSED, got %v", mon.State().Lid)
	}
	mon.Stop()
}

// TestThermalShedHysteresis checks THERMAL_SHED fires when headroom is tight and
// only clears (WAKE) once there is ample headroom — no flapping.
func TestThermalShedHysteresis(t *testing.T) {
	rec := &recorder{}
	src := lifecycle.NewChannelSource()

	mon := lifecycle.NewMonitor(&mockCrdt{}, []byte("doc-thermal"))
	mon.SetSource(src)
	mon.OnTransition(rec.on)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mon.Start(ctx)

	// Tight headroom → shed.
	src.Push(lifecycle.PowerEvent{Kind: lifecycle.EvThermal, Thermal: contract.Thermal{HeadroomC: 3}})
	waitFor(t, func() bool { return rec.count(lifecycle.ThermalShed) == 1 })

	// Still warm (between thresholds) → must not flap a second shed.
	src.Push(lifecycle.PowerEvent{Kind: lifecycle.EvThermal, Thermal: contract.Thermal{HeadroomC: 8}})
	// Ample headroom → clear.
	src.Push(lifecycle.PowerEvent{Kind: lifecycle.EvThermal, Thermal: contract.Thermal{HeadroomC: 20}})
	waitFor(t, func() bool { return rec.count(lifecycle.Wake) == 1 })

	if got := rec.count(lifecycle.ThermalShed); got != 1 {
		t.Errorf("expected exactly 1 THERMAL_SHED, got %d", got)
	}
	mon.Stop()
}

// TestCheckpointFailureSurfaced ensures a CRDT checkpoint error aborts the
// hand-back and is returned from PrepareSleep (don't promote a standby off a
// failed checkpoint).
func TestCheckpointFailureSurfaced(t *testing.T) {
	crdt := &mockCrdt{failNext: true}
	coord := &fakeCoordinator{}
	mon := lifecycle.NewMonitor(crdt, []byte("doc-fail"))
	mon.SetCoordinator(coord)

	if err := mon.PrepareSleep(); err == nil {
		t.Fatal("expected PrepareSleep to surface the checkpoint error")
	}
	if coord.prepareCount() != 0 {
		t.Errorf("coordinator must not be called when checkpoint fails, got %d", coord.prepareCount())
	}
}

// TestNoCoordinatorIsSafe confirms the monitor still works with no scheduler
// registered (standalone, per CONTRACT.md §3).
func TestNoCoordinatorIsSafe(t *testing.T) {
	crdt := &mockCrdt{}
	rec := &recorder{}
	mon := lifecycle.NewMonitor(crdt, []byte("doc-solo"))
	mon.OnTransition(rec.on)

	if err := mon.PrepareSleep(); err != nil {
		t.Fatalf("PrepareSleep without coordinator: %v", err)
	}
	if !rec.has(lifecycle.SleepImminent) || crdt.count() != 1 {
		t.Error("expected SLEEP_IMMINENT + checkpoint even without a coordinator")
	}
	if err := mon.Resume(); err != nil {
		t.Fatalf("Resume without coordinator: %v", err)
	}
}
