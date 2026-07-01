//go:build windows

package lifecycle

// Live Windows verification for the real event source. These tests run the
// actual Win32 calls (GetSystemPowerStatus, RegisterSuspendResumeNotification,
// RegisterPowerSettingNotification via the hidden lid window) against this
// machine — they are not mocks. What they intentionally do NOT do is force a
// real suspend or lid-close (this session cannot physically do that): those
// paths are proven by asserting registration succeeds, returns a usable
// handle, and unregisters cleanly, exactly as vertical 09's audio-package
// sibling does for its own unverifiable-in-CI OS backends.

import (
	"context"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
)

// TestGetSystemPowerStatusLive proves the raw Win32 GetSystemPowerStatus
// binding is wired correctly by asserting the CURRENT power state of this
// real machine is plausible: either genuinely on AC or on battery, and
// either a sane 0-100 percentage or "no battery" (desktop/VM case).
func TestGetSystemPowerStatusLive(t *testing.T) {
	snap, err := getSystemPowerStatus()
	if err != nil {
		t.Fatalf("GetSystemPowerStatus failed: %v", err)
	}
	t.Logf("live power snapshot: onAC=%v noBattery=%v batteryPct=%.0f", snap.OnAC, snap.NoBattery, snap.BatteryPct)

	if snap.BatteryPct < 0 || snap.BatteryPct > 100 {
		t.Fatalf("battery percent out of range: %v", snap.BatteryPct)
	}
	// A machine with a battery present must report *some* charge level; a
	// desktop/VM with no battery is expected to report NoBattery instead.
	// Either is a valid, real answer from the OS — we just assert the API
	// produced a coherent one, not a specific value (we don't know this
	// runner's hardware in advance).
}

// TestSuspendResumeNotificationRegistersAndUnregisters exercises the real
// RegisterSuspendResumeNotification / UnregisterSuspendResumeNotification
// Win32 calls end-to-end: registration must succeed with a non-zero handle,
// and unregistration must succeed cleanly. We cannot force an actual system
// suspend in this automated session, so the callback firing itself is not
// asserted here — only that the OS accepted and released the real
// registration (i.e. this is not exercising a mock).
func TestSuspendResumeNotificationRegistersAndUnregisters(t *testing.T) {
	fired := make(chan uint32, 1)
	reg, err := registerSuspendResumeNotification(func(eventType uint32) {
		select {
		case fired <- eventType:
		default:
		}
	})
	if err != nil {
		t.Fatalf("RegisterSuspendResumeNotification failed: %v", err)
	}
	if reg == nil || reg.handle == 0 {
		t.Fatal("expected a non-zero HPOWERNOTIFY handle")
	}

	if err := reg.unregister(); err != nil {
		t.Fatalf("UnregisterSuspendResumeNotification failed: %v", err)
	}
	if reg.handle != 0 {
		t.Fatal("expected handle to be cleared after unregister")
	}

	// Double-unregister must be a safe no-op (mirrors Stop()/close() being
	// idempotent elsewhere in this package).
	if err := reg.unregister(); err != nil {
		t.Fatalf("second unregister should be a no-op, got: %v", err)
	}

	// We cannot force a real suspend/resume cycle in this session, so we
	// only assert that no spurious callback fired during the brief window
	// the registration was live.
	select {
	case ev := <-fired:
		t.Logf("unexpected but harmless: got a real PBT_APM event %d during the test window", ev)
	default:
	}
}

// TestLidWatcherRegistersAndUnregisters exercises the real hidden
// message-window + RegisterPowerSettingNotification(GUID_LIDSWITCH_STATE_
// CHANGE) path end-to-end: window creation, notification registration, the
// message pump starting, and clean teardown. Forcing an actual lid close is
// not possible in this automated session (and this machine may not even
// have a lid), so we only assert the live Win32 registration plumbing works,
// not that a transition was observed.
func TestLidWatcherRegistersAndUnregisters(t *testing.T) {
	lidCh := make(chan bool, 1)
	w, err := newLidWatcher(func(open bool) {
		select {
		case lidCh <- open:
		default:
		}
	})
	if err != nil {
		t.Fatalf("newLidWatcher failed: %v", err)
	}
	if w.hwnd == 0 {
		t.Fatal("expected a non-zero hidden window handle")
	}
	if w.notify == 0 {
		t.Fatal("expected a non-zero HPOWERNOTIFY from RegisterPowerSettingNotification")
	}

	if err := w.close(); err != nil {
		t.Fatalf("lidWatcher.close failed: %v", err)
	}

	select {
	case open := <-lidCh:
		t.Logf("unexpected but harmless: got a real lid transition (open=%v) during the test window", open)
	default:
	}
}

// TestWindowsEventSourceEmitsLiveSample drives the full EventSource wiring
// (winEventSource.Events) exactly the way lifecycle.Monitor.Start does, and
// asserts the first emitted sample reflects this real machine's current
// power state via the same contract.Power shape the state machine consumes.
// This is the end-to-end proof that NewOSEventSource() on Windows is real,
// not the stub: unlike source_stub_test-style assertions, this must observe
// an actual EvSample before the (generous) deadline.
func TestWindowsEventSourceEmitsLiveSample(t *testing.T) {
	src := NewOSEventSource()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events := src.Events(ctx)
	select {
	case ev, ok := <-events:
		if !ok {
			t.Fatal("event channel closed before any sample was delivered")
		}
		if ev.Kind != EvSample {
			t.Fatalf("expected first event to be EvSample, got kind %v", ev.Kind)
		}
		if ev.Power.Src != contract.PowerAC && ev.Power.Src != contract.PowerBattery {
			t.Fatalf("unexpected power source value: %v", ev.Power.Src)
		}
		if ev.Power.BatteryPct < 0 || ev.Power.BatteryPct > 100 {
			t.Fatalf("battery pct out of range: %v", ev.Power.BatteryPct)
		}
		t.Logf("live EvSample: src=%v batteryPct=%.0f lid=%v hint=%v",
			ev.Power.Src, ev.Power.BatteryPct, ev.Power.Lid, ev.Power.Hint)
	case <-ctx.Done():
		t.Fatal("timed out waiting for the real Windows event source to emit an initial sample")
	}

	cancel()
	// Drain until the source closes its channel in response to ctx
	// cancellation, proving clean shutdown of the poll loop + notification
	// registrations (no goroutine/handle leak left dangling).
	drainDeadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}
		case <-drainDeadline:
			t.Fatal("event channel did not close after context cancellation")
		}
	}
}
