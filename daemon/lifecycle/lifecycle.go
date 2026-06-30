// Package lifecycle implements vertical 09 — Power, Thermal & Sleep.
//
// The target nodes are laptops: they throttle, sleep, and have their lids closed
// mid-pipeline. This package makes node lifecycle a first-class scheduler input.
// It runs a small state machine over an INJECTABLE event source (real OS hooks
// are a labeled stub in source.go) and, on imminent sleep, drives the "lid-drop"
// recovery choreography from ARCHITECTURE §4.2:
//
//	checkpoint CRDT memory  →  hand back capabilities  →  promote a hot standby
//
// before the node goes dark. States/events align with ARCHITECTURE §3.2
// (SleepHint AWAKE/IDLE/SLEEP_IMMINENT, LidState OPEN/CLOSED, PowerSource AC/BATTERY).
package lifecycle

import (
	"context"
	"sync"

	contract "github.com/hash066/cerberus/contract/go"
)

// LifecycleEvent is a transition observers (the scheduler, tray, supervisor) can
// subscribe to via OnTransition. The string values mirror schemas §8 topics
// published on cerberus/<site>/lifecycle/<peer>.
type LifecycleEvent string

const (
	// ThermalShed: approaching thermal limits — scheduler should shed load to
	// cooler nodes (ARCHITECTURE §3, vertical 09 §3).
	ThermalShed LifecycleEvent = "THERMAL_SHED"
	// SleepImminent: the node is about to go dark — checkpoint + hand-back + standby.
	SleepImminent LifecycleEvent = "SLEEP_IMMINENT"
	// LidClosed: the lid was closed — on laptops this is treated as imminent sleep.
	LidClosed LifecycleEvent = "LID_CLOSED"
	// Wake: the node resumed (or thermal/lid pressure cleared) — re-advertise + merge.
	Wake LifecycleEvent = "WAKE"
)

// Monitor is the lifecycle state machine. It is safe for concurrent use: the
// drain loop and external callers (PrepareSleep/Resume/State) all go through mu.
type Monitor struct {
	crdt  contract.CrdtEngine
	docID []byte

	mu          sync.Mutex
	power       contract.Power   // current best-known power snapshot
	thermal     contract.Thermal // current best-known thermal snapshot
	listeners   []func(LifecycleEvent)
	coordinator StandbyCoordinator
	source      EventSource
	heldCaps    [][]byte // capabilities granted to this node, handed back on sleep
	thermalShed bool     // hysteresis latch for THERMAL_SHED
	cancel      context.CancelFunc
}

// NewMonitor constructs a Monitor bound to a CRDT engine (for sleep checkpoints)
// and a memory document id. The default event source is the OS-backed stub
// (NewOSEventSource); inject a ChannelSource via SetSource to drive it
// deterministically (tests, simulation).
func NewMonitor(crdt contract.CrdtEngine, docID []byte) *Monitor {
	return &Monitor{
		crdt:   crdt,
		docID:  docID,
		power:  contract.Power{Src: contract.PowerAC, BatteryPct: 100, Lid: contract.LidOpen, Hint: contract.SleepAwake},
		source: NewOSEventSource(),
	}
}

// SetSource swaps the event source. Call before Start. Passing nil is a no-op.
func (m *Monitor) SetSource(s EventSource) {
	if s == nil {
		return
	}
	m.mu.Lock()
	m.source = s
	m.mu.Unlock()
}

// SetCoordinator registers the scheduler's StandbyCoordinator. The monitor calls
// it during the SLEEP_IMMINENT / WAKE choreography. Defined consumer-side so this
// package never imports the scheduler.
func (m *Monitor) SetCoordinator(c StandbyCoordinator) {
	m.mu.Lock()
	m.coordinator = c
	m.mu.Unlock()
}

// SetHeldCaps records the capability handles granted to this node, so they can be
// handed back (revoked/closed) before sleep — no dangling authority (vertical 09 §7).
func (m *Monitor) SetHeldCaps(caps [][]byte) {
	m.mu.Lock()
	m.heldCaps = caps
	m.mu.Unlock()
}

// OnTransition registers a listener for lifecycle events.
func (m *Monitor) OnTransition(fn func(LifecycleEvent)) {
	m.mu.Lock()
	m.listeners = append(m.listeners, fn)
	m.mu.Unlock()
}

// State returns the current power snapshot (ARCHITECTURE §3.2 fields).
func (m *Monitor) State() contract.Power {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.power
}

// Thermal returns the current best-known thermal snapshot.
func (m *Monitor) Thermal() contract.Thermal {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.thermal
}

// Start begins draining the injected event source until ctx (or Stop) cancels.
func (m *Monitor) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	m.cancel = cancel
	src := m.source
	m.mu.Unlock()

	go m.run(ctx, src)
}

func (m *Monitor) run(ctx context.Context, src EventSource) {
	if src == nil {
		<-ctx.Done()
		return
	}
	events := src.Events(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			m.handle(ev)
		}
	}
}

// handle folds one raw power event into the state machine.
func (m *Monitor) handle(ev PowerEvent) {
	switch ev.Kind {
	case EvSample:
		m.mu.Lock()
		m.power = ev.Power
		m.mu.Unlock()
		// A sample may itself announce imminent sleep.
		if ev.Power.Hint == contract.SleepImminent {
			_ = m.prepareSleep(ReasonSleepImminent)
		}
	case EvThermal:
		m.onThermal(ev.Thermal)
	case EvLidClosed:
		m.mu.Lock()
		m.power.Lid = contract.LidClosed
		m.mu.Unlock()
		m.emit(LidClosed)
		// On laptops a closed lid means imminent sleep (the lid-drop trigger).
		_ = m.prepareSleep(ReasonLidClosed)
	case EvLidOpened:
		m.mu.Lock()
		m.power.Lid = contract.LidOpen
		m.mu.Unlock()
	case EvSleepImminent:
		_ = m.prepareSleep(ReasonSleepImminent)
	case EvWake:
		_ = m.resume()
	}
}

// onThermal applies hysteresis: shed when headroom is tight (or already
// throttling), clear only when there is ample headroom again — so THERMAL_SHED /
// WAKE don't flap around a single threshold.
func (m *Monitor) onThermal(t contract.Thermal) {
	const shedAt, clearAbove = 5.0, 15.0 // headroom °C: shed when tight, clear when ample
	m.mu.Lock()
	m.thermal = t
	shed := m.thermalShed
	hot := t.Throttling || t.HeadroomC <= shedAt
	cool := !t.Throttling && t.HeadroomC >= clearAbove
	switch {
	case hot && !shed:
		m.thermalShed = true
		m.mu.Unlock()
		m.emit(ThermalShed)
		return
	case cool && shed:
		m.thermalShed = false
		m.mu.Unlock()
		m.emit(Wake)
		return
	}
	m.mu.Unlock()
}

// PrepareSleep runs the imminent-sleep choreography (manual entry point, e.g. an
// operator command or the daemon shutting down). It is the same path the event
// source triggers on SleepHint=SLEEP_IMMINENT or a lid close.
func (m *Monitor) PrepareSleep() error { return m.prepareSleep(ReasonManual) }

// prepareSleep: emit SLEEP_IMMINENT → checkpoint CRDT → hand back caps + promote
// a standby via the coordinator. Order matches ARCHITECTURE §4.2.
func (m *Monitor) prepareSleep(reason SleepReason) error {
	m.mu.Lock()
	m.power.Hint = contract.SleepImminent
	coord := m.coordinator
	caps := m.heldCaps
	docID := m.docID
	crdt := m.crdt
	m.mu.Unlock()

	// 1. Pre-announce so subscribers (scheduler/supervisor/tray) react before dark.
	m.emit(SleepImminent)

	// 2. Checkpoint the CRDT memory doc (seal the vector clock, flush deltas).
	if crdt != nil {
		if _, err := crdt.Checkpoint(docID); err != nil {
			return err
		}
	}

	// 3. Hand back capabilities + ask the scheduler to promote a hot standby.
	if coord != nil {
		return coord.HandBackAndPromote(SleepPrepare{
			DocID:    docID,
			Reason:   reason,
			HeldCaps: caps,
		})
	}
	return nil
}

// Resume runs the wake choreography: clear the sleep hint, tell the coordinator
// to release the standby, and emit WAKE so the node re-advertises + CRDT-merges.
func (m *Monitor) Resume() error { return m.resume() }

func (m *Monitor) resume() error {
	m.mu.Lock()
	m.power.Hint = contract.SleepAwake
	m.power.Lid = contract.LidOpen
	coord := m.coordinator
	docID := m.docID
	m.mu.Unlock()

	m.emit(Wake)
	if coord != nil {
		return coord.Resume(WakePrepare{DocID: docID})
	}
	return nil
}

// emit fans an event out to all listeners. Listeners are snapshotted under the
// lock and invoked without it held, so a listener may call back into the monitor.
func (m *Monitor) emit(ev LifecycleEvent) {
	m.mu.Lock()
	ls := make([]func(LifecycleEvent), len(m.listeners))
	copy(ls, m.listeners)
	m.mu.Unlock()
	for _, l := range ls {
		l(ev)
	}
}

// Stop cancels the drain loop.
func (m *Monitor) Stop() {
	m.mu.Lock()
	cancel := m.cancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
