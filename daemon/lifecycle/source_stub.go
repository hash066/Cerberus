//go:build !windows

package lifecycle

import "context"

// ---------------------------------------------------------------------------
// OS-backed source: STUB for every non-Windows GOOS (the only OS-integration
// point in this package on these platforms).
//
// osEventSource is where real OS power/thermal hooks belong (vertical 09 §6):
//   - macOS: IOKit / IOPMPowerSource + thermal-pressure notifications
//   - Linux: /sys/class/power_supply, /sys/class/thermal, systemd-logind sleep
//     inhibitors
//
// Neither is wired here — doing so portably needs cgo/platform code that is
// explicitly out of scope for v0.1 (MATURITY HONESTY: this is a documented
// stub, not a faked working implementation). For now it produces no events;
// the daemon drives lifecycle through the injectable ChannelSource /
// PrepareSleep. Windows gets a real implementation — see source_windows.go.
// ---------------------------------------------------------------------------

type osEventSource struct{}

// NewOSEventSource returns the (stub) OS-backed source on this platform. It
// currently emits nothing; swap in real platform hooks here to make lifecycle
// OS-driven. On Windows this function instead returns a real event source —
// see source_windows.go.
func NewOSEventSource() EventSource { return osEventSource{} }

func (osEventSource) Events(ctx context.Context) <-chan PowerEvent {
	ch := make(chan PowerEvent)
	go func() {
		// STUB: no real OS power/thermal hooks yet on this platform. Block
		// until cancellation so the monitor's drain loop has a well-behaved
		// channel to select on.
		<-ctx.Done()
		close(ch)
	}()
	return ch
}
