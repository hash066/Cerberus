package lifecycle

import (
	"context"

	contract "github.com/hash066/cerberus/contract/go"
)

// PowerEventKind classifies a raw signal coming from the host's power/thermal
// subsystem. The lifecycle monitor folds a stream of these into the higher-level
// state machine (AWAKE/IDLE/SLEEP_IMMINENT + thermal shed + lid) and emits the
// recovery LifecycleEvents the scheduler subscribes to.
type PowerEventKind uint8

const (
	// EvSample carries a fresh full Power snapshot (battery %, source, lid, hint).
	EvSample PowerEventKind = iota
	// EvThermal carries a fresh Thermal snapshot (drives THERMAL_SHED).
	EvThermal
	// EvLidClosed / EvLidOpened report a lid transition.
	EvLidClosed
	EvLidOpened
	// EvSleepImminent is the OS pre-announcing imminent sleep (the predictable,
	// graceful path — see ARCHITECTURE §4.2). It is the "lid-drop" recovery trigger.
	EvSleepImminent
	// EvWake reports the node has resumed from sleep.
	EvWake
)

// PowerEvent is one raw observation from the EventSource. Only the fields
// relevant to Kind are meaningful: the monitor reads Power for EvSample and
// Thermal for EvThermal, and treats the lid/sleep/wake kinds as bare transitions.
type PowerEvent struct {
	Kind    PowerEventKind
	Power   contract.Power
	Thermal contract.Thermal
}

// EventSource is the injectable seam between the OS power/thermal subsystem and
// the lifecycle state machine. Production builds plug in a real OS-backed source
// (IOKit / Win32 power notifications / sysfs + logind — see vertical 09 §6);
// tests and the simulated path plug in ChannelSource. The state machine itself
// is identical in both cases, so the OS integration is the only stub.
//
// Events returns a channel the monitor drains until ctx is cancelled. The source
// owns the channel and SHOULD close it when ctx is done.
type EventSource interface {
	Events(ctx context.Context) <-chan PowerEvent
}

// ChannelSource is a fully in-memory EventSource used by tests and the simulated
// driver. Callers Push() raw events and the monitor consumes them — no OS hooks,
// so the state machine can be exercised deterministically.
type ChannelSource struct {
	ch chan PowerEvent
}

// NewChannelSource returns a ChannelSource with a small buffer.
func NewChannelSource() *ChannelSource {
	return &ChannelSource{ch: make(chan PowerEvent, 16)}
}

// Push enqueues a raw power event for the monitor to fold in.
func (s *ChannelSource) Push(ev PowerEvent) { s.ch <- ev }

// Events implements EventSource.
func (s *ChannelSource) Events(ctx context.Context) <-chan PowerEvent { return s.ch }

// ---------------------------------------------------------------------------
// OS-backed source: STUB (the only OS-integration point in this package).
//
// osEventSource is where real OS power/thermal hooks belong (vertical 09 §6):
//   - macOS:   IOKit / IOPMPowerSource + thermal-pressure notifications
//   - Windows: RegisterPowerSettingNotification / GetSystemPowerStatus, WM_POWERBROADCAST
//   - Linux:   /sys/class/power_supply, /sys/class/thermal, systemd-logind sleep inhibitors
//
// None of those are wired here — doing so portably needs cgo/platform code that
// is explicitly out of scope for v0.1 (MATURITY HONESTY: this is a documented
// stub, not a faked working implementation). For now it produces no events; the
// daemon drives lifecycle through the injectable ChannelSource / PrepareSleep.
// ---------------------------------------------------------------------------

type osEventSource struct{}

// NewOSEventSource returns the (stub) OS-backed source. It currently emits
// nothing; swap in real platform hooks here to make lifecycle OS-driven.
func NewOSEventSource() EventSource { return osEventSource{} }

func (osEventSource) Events(ctx context.Context) <-chan PowerEvent {
	ch := make(chan PowerEvent)
	go func() {
		// STUB: no real OS power/thermal hooks yet. Block until cancellation so
		// the monitor's drain loop has a well-behaved channel to select on.
		<-ctx.Done()
		close(ch)
	}()
	return ch
}
