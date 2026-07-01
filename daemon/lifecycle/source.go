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
// OS-backed source: the per-platform hook lives in source_windows.go on
// Windows (real Win32 APIs: GetSystemPowerStatus + RegisterSuspendResume-
// Notification + RegisterPowerSettingNotification — see that file for the
// design rationale) and in source_stub.go on every other GOOS (vertical 09 §6):
//   - macOS: IOKit / IOPMPowerSource + thermal-pressure notifications — not yet
//     wired (MATURITY HONESTY: documented stub, not a faked backend).
//   - Linux: /sys/class/power_supply, /sys/class/thermal, systemd-logind sleep
//     inhibitors — not yet wired (same honesty note).
//
// Both files expose the same NewOSEventSource() EventSource entry point so
// NewMonitor (lifecycle.go) is identical on every platform; only the source
// behind it differs.
// ---------------------------------------------------------------------------
