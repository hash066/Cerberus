//go:build windows

package lifecycle

// Real Windows event source for vertical 09 (Power, Thermal & Sleep). This
// replaces the injectable-only stub (source_stub.go, still used on
// !windows) with genuine Win32 signals, wired behind the same EventSource
// seam so the lifecycle.Monitor state machine is unchanged on this platform:
//
//   - AC/battery + battery %: polled from GetSystemPowerStatus (kernel32) on
//     a short interval. A hidden-window WM_POWERBROADCAST loop could also
//     deliver PBT_APMPOWERSTATUSCHANGE for this, but it would still just
//     turn around and call GetSystemPowerStatus to get the actual numbers —
//     polling a few-second ticker is simpler, has no window/message-pump
//     dependency, and battery percentage does not need sub-second latency,
//     so that is the tradeoff made here.
//   - Sleep-imminent / wake: RegisterSuspendResumeNotification (user32 —
//     see winpower_windows.go for why, despite the Powrprof.h header MSDN
//     documents it under), the callback-based API. Chosen over standing up
//     a message-loop window and watching WM_POWERBROADCAST/PBT_APMSUSPEND
//     because it needs no window at all — Windows invokes the callback
//     directly on suspend (PBT_APMSUSPEND) and resume
//     (PBT_APMRESUMESUSPEND / PBT_APMRESUMEAUTOMATIC). This is the
//     "simpler fit" the task calls for.
//   - Lid open/closed: RegisterPowerSettingNotification with
//     GUID_LIDSWITCH_STATE_CHANGE. Unlike suspend/resume, MSDN only defines
//     window-handle and service-handle recipients for power-setting
//     notifications — there is no callback-only variant — so this signal
//     alone gets a hidden, message-only window (HWND_MESSAGE) with a minimal
//     GetMessage/DispatchMessage pump on a dedicated, thread-locked
//     goroutine (see winpower_windows.go). Using different mechanisms for
//     different signals is intentional: each is the genuinely correct Win32
//     API for that signal, not a one-size-fits-all approximation.

import (
	"context"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
)

// pollInterval is how often we re-poll GetSystemPowerStatus for AC/battery
// changes. A few seconds is plenty responsive for a scheduler input that
// drives load-shedding/checkpoint decisions, not a UI battery meter.
const pollInterval = 5 * time.Second

// winEventSource is the real Windows-backed EventSource: it polls
// GetSystemPowerStatus and fans out suspend/resume + lid notifications, all
// translated into the same PowerEvent stream the (platform-agnostic)
// lifecycle.Monitor already knows how to fold.
type winEventSource struct{}

// NewOSEventSource returns the real Windows event source. It is the
// Windows-only replacement for the injectable-only stub: production code on
// this platform gets genuine Win32 power/lid/sleep signals, while
// tests/simulation continue to use SetSource(ChannelSource) exactly as
// before — the state machine in lifecycle.go is untouched.
func NewOSEventSource() EventSource { return winEventSource{} }

func (winEventSource) Events(ctx context.Context) <-chan PowerEvent {
	ch := make(chan PowerEvent, 8)

	go func() {
		defer close(ch)

		// Track lid state across polls/notifications so a fresh GetSystem-
		// PowerStatus sample doesn't clobber what the lid watcher last saw
		// (GetSystemPowerStatus does not report lid state at all).
		lastLid := contract.LidOpen

		send := func(ev PowerEvent) {
			select {
			case ch <- ev:
			case <-ctx.Done():
			}
		}

		emitSample := func() {
			snap, err := getSystemPowerStatus()
			if err != nil {
				// A transient failure to query power status is not fatal —
				// just skip this tick, the next poll will retry.
				return
			}
			p := contract.Power{
				Src:        contract.PowerBattery,
				BatteryPct: snap.BatteryPct,
				Lid:        lastLid,
				Hint:       contract.SleepAwake,
			}
			if snap.OnAC {
				p.Src = contract.PowerAC
			}
			if snap.NoBattery {
				p.BatteryPct = 100 // desktop / no battery: report full, never phantom-low
			}
			send(PowerEvent{Kind: EvSample, Power: p})
		}

		// Suspend/resume via the callback-based API — no window required.
		suspendResume, srErr := registerSuspendResumeNotification(func(eventType uint32) {
			switch eventType {
			case pbtAPMSuspend:
				send(PowerEvent{Kind: EvSleepImminent})
			case pbtAPMResumeSuspend, pbtAPMResumeAutomatic:
				send(PowerEvent{Kind: EvWake})
			}
		})
		if srErr == nil {
			defer suspendResume.unregister()
		}

		// Lid open/closed via the hidden message-window watcher (the only
		// correct mechanism for GUID_LIDSWITCH_STATE_CHANGE).
		lw, lidErr := newLidWatcher(func(open bool) {
			if open {
				lastLid = contract.LidOpen
				send(PowerEvent{Kind: EvLidOpened})
				return
			}
			lastLid = contract.LidClosed
			send(PowerEvent{Kind: EvLidClosed})
		})
		if lidErr == nil {
			defer lw.close()
		}

		// Emit an initial sample immediately so callers see current state
		// without waiting a full poll interval.
		emitSample()

		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				emitSample()
			}
		}
	}()

	return ch
}
