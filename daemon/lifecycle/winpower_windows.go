//go:build windows

package lifecycle

// Raw Win32 bindings for the lifecycle event source. golang.org/x/sys/windows
// (already a module dependency — see go.mod) does not expose
// GetSystemPowerStatus, RegisterSuspendResumeNotification, or the
// RegisterPowerSettingNotification / message-window plumbing needed for lid
// state, so this file declares the procs, structs, and constants by hand via
// windows.NewLazySystemDLL, following the same pattern already used in
// daemon/discovery/process_windows.go for OpenProcess/GetExitCodeProcess.
//
// References: MSDN "GetSystemPowerStatus" (kernel32.dll),
// "RegisterSuspendResumeNotification" / "RegisterPowerSettingNotification"
// (documented under Powrprof.h but actually exported by user32.dll for the
// HWND/callback desktop-app recipient — confirmed live against this
// machine's user32.dll via GetProcAddress before wiring this up), plus
// "WM_POWERBROADCAST" / "GUID_LIDSWITCH_STATE_CHANGE" (ARCHITECTURE §3.2 /
// vertical 09 §6).

import (
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Despite Powrprof.h being the header MSDN documents RegisterSuspendResume-
// Notification/RegisterPowerSettingNotification under, both are actually
// exported by user32.dll for desktop-app (HWND/callback) recipients — verified
// live against this machine's real user32.dll/kernel32.dll via GetProcAddress
// before wiring this up (kernel32.dll does NOT export either symbol here).
var (
	modkernel32Power = windows.NewLazySystemDLL("kernel32.dll")
	moduser32Power   = windows.NewLazySystemDLL("user32.dll")

	procGetSystemPowerStatus = modkernel32Power.NewProc("GetSystemPowerStatus")

	procRegisterSuspendResumeNotification   = moduser32Power.NewProc("RegisterSuspendResumeNotification")
	procUnregisterSuspendResumeNotification = moduser32Power.NewProc("UnregisterSuspendResumeNotification")

	procRegisterPowerSettingNotification   = moduser32Power.NewProc("RegisterPowerSettingNotification")
	procUnregisterPowerSettingNotification = moduser32Power.NewProc("UnregisterPowerSettingNotification")
	procRegisterClassExW                   = moduser32Power.NewProc("RegisterClassExW")
	procCreateWindowExW                    = moduser32Power.NewProc("CreateWindowExW")
	procDestroyWindow                      = moduser32Power.NewProc("DestroyWindow")
	procDefWindowProcW                     = moduser32Power.NewProc("DefWindowProcW")
	procGetMessageW                        = moduser32Power.NewProc("GetMessageW")
	procTranslateMessage                   = moduser32Power.NewProc("TranslateMessage")
	procDispatchMessageW                   = moduser32Power.NewProc("DispatchMessageW")
	procPostMessageW                       = moduser32Power.NewProc("PostMessageW")
	procPostQuitMessage                    = moduser32Power.NewProc("PostQuitMessage")
)

// --- GetSystemPowerStatus ----------------------------------------------------

// systemPowerStatus mirrors the Win32 SYSTEM_POWER_STATUS struct exactly
// (field order/size matter for the raw syscall).
type systemPowerStatus struct {
	ACLineStatus        byte
	BatteryFlag         byte
	BatteryLifePercent  byte
	SystemStatusFlag    byte // reserved on desktop; "battery saver" on newer SDKs
	BatteryLifeTime     uint32
	BatteryFullLifeTime uint32
}

const (
	acLineOffline         = 0
	acLineOnline          = 1
	acLineUnknown         = 255
	batteryFlagNoBattery  = 128
	batteryFlagUnknown    = 255
	batteryPercentUnknown = 255
)

// winPowerSnapshot is the decoded result of one GetSystemPowerStatus call —
// an internal shape (not the frozen contract.Power) that source_windows.go
// folds into a contract.Power sample alongside the best-known lid/sleep hint.
type winPowerSnapshot struct {
	OnAC       bool
	NoBattery  bool
	BatteryPct float64
}

// getSystemPowerStatus calls the real Win32 GetSystemPowerStatus and decodes
// AC-line status + battery percentage. Lid state and sleep-imminent are NOT
// reported by this API — those come from the notification callbacks
// (registerSuspendResumeNotification / lidWatcher) below.
func getSystemPowerStatus() (winPowerSnapshot, error) {
	var sps systemPowerStatus
	r1, _, err := procGetSystemPowerStatus.Call(uintptr(unsafe.Pointer(&sps)))
	if r1 == 0 {
		return winPowerSnapshot{}, err
	}
	snap := winPowerSnapshot{}
	switch sps.ACLineStatus {
	case acLineOnline:
		snap.OnAC = true
	case acLineOffline:
		snap.OnAC = false
	default: // acLineUnknown: fall back to "battery present?" heuristic
		snap.OnAC = sps.BatteryFlag == batteryFlagNoBattery
	}
	snap.NoBattery = sps.BatteryFlag == batteryFlagNoBattery || sps.BatteryFlag == batteryFlagUnknown
	if sps.BatteryLifePercent != batteryPercentUnknown {
		snap.BatteryPct = float64(sps.BatteryLifePercent)
	} else {
		snap.BatteryPct = 100 // unknown -> report full so we never manufacture a low-battery signal
	}
	return snap, nil
}

// --- RegisterSuspendResumeNotification (callback-based, no window needed) --

const (
	deviceNotifyCallback = 2 // DEVICE_NOTIFY_CALLBACK

	pbtAPMSuspend         = 4  // PBT_APMSUSPEND: sleep is imminent
	pbtAPMResumeSuspend   = 7  // PBT_APMRESUMESUSPEND: resumed from a suspend
	pbtAPMResumeAutomatic = 18 // PBT_APMRESUMEAUTOMATIC: resumed (may still be user-absent)
)

// deviceNotifySubscribeParameters mirrors DEVICE_NOTIFY_SUBSCRIBE_PARAMETERS.
type deviceNotifySubscribeParameters struct {
	Callback uintptr
	Context  uintptr
}

// suspendResumeRegistration wraps the callback-based
// RegisterSuspendResumeNotification handle so callers can cleanly unregister.
type suspendResumeRegistration struct {
	handle   uintptr
	callback uintptr // keep the NewCallback uintptr alive for the registration's lifetime
}

// registerSuspendResumeNotification wires fn to be invoked (on an arbitrary OS
// thread, per MSDN) with the PBT_APM* event type whenever Windows suspends or
// resumes this session. It requires no message-loop window — the simpler of
// the two mechanisms discussed for sleep-imminent/wake, which is why it is
// used here instead of watching WM_POWERBROADCAST on the hidden window.
func registerSuspendResumeNotification(fn func(eventType uint32)) (*suspendResumeRegistration, error) {
	cb := windows.NewCallback(func(context uintptr, eventType uint32, setting uintptr) uintptr {
		fn(eventType)
		return 0
	})
	params := deviceNotifySubscribeParameters{Callback: cb}
	h, _, err := procRegisterSuspendResumeNotification.Call(
		uintptr(unsafe.Pointer(&params)),
		uintptr(deviceNotifyCallback),
	)
	if h == 0 {
		return nil, err
	}
	return &suspendResumeRegistration{handle: h, callback: cb}, nil
}

// unregister cleanly tears down the suspend/resume registration.
func (r *suspendResumeRegistration) unregister() error {
	if r == nil || r.handle == 0 {
		return nil
	}
	ret, _, err := procUnregisterSuspendResumeNotification.Call(r.handle)
	r.handle = 0
	if ret == 0 {
		return err
	}
	return nil
}

// --- Lid state: RegisterPowerSettingNotification needs a message window ----
//
// Unlike suspend/resume, lid-open/closed is only delivered as
// WM_POWERBROADCAST / PBT_POWERSETTINGCHANGE for the GUID_LIDSWITCH_STATE_
// CHANGE power setting, which — per MSDN's RegisterPowerSettingNotification —
// requires a window handle (or a service status handle) to receive it: there
// is no window-free callback variant for power-setting notifications the way
// there is for suspend/resume. So lid tracking gets its own hidden,
// message-only window with a minimal message pump running on a dedicated,
// locked OS thread (Win32 window/message APIs are thread-affine).

// guidLidSwitchStateChange is GUID_LIDSWITCH_STATE_CHANGE
// ({BA3E0F4D-B817-4094-A2D1-D56379E6A0F3}).
var guidLidSwitchStateChange = windows.GUID{
	Data1: 0xBA3E0F4D,
	Data2: 0xB817,
	Data3: 0x4094,
	Data4: [8]byte{0xA2, 0xD1, 0xD5, 0x63, 0x79, 0xE6, 0xA0, 0xF3},
}

const (
	wmPowerBroadcast      = 0x0218
	wmDestroy             = 0x0002
	wmClose               = 0x0010
	pbtPowerSettingChange = 0x8013

	deviceNotifyWindowHandle = 0 // DEVICE_NOTIFY_WINDOW_HANDLE

	hwndMessageOnly = ^uintptr(0) - 2 // HWND_MESSAGE == (HWND)-3, sign-extended: max-uintptr minus 2
	cwUseDefault    = ^uint32(0x7FFFFFFF)
	wsOverlapped    = 0
)

// powerBroadcastSetting mirrors the fixed-size header of POWERBROADCAST_SETTING
// (PowerSetting GUID + DataLength + a 1-byte Data[] payload, which is all a lid
// notification ever carries: 0 = closed, non-zero = open).
type powerBroadcastSetting struct {
	PowerSetting windows.GUID
	DataLength   uint32
	Data         [1]byte
}

// wndClassExW mirrors WNDCLASSEXW.
type wndClassExW struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   windows.Handle
	Icon       windows.Handle
	Cursor     windows.Handle
	Background windows.Handle
	MenuName   *uint16
	ClassName  *uint16
	IconSm     windows.Handle
}

// msg mirrors MSG.
type msg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
}

// lidWatcher owns the hidden message-only window used solely to receive
// WM_POWERBROADCAST/PBT_POWERSETTINGCHANGE for the lid switch. It runs its
// message pump on a dedicated goroutine locked to one OS thread, as Win32
// requires the thread that creates a window to be the one that pumps its
// messages.
type lidWatcher struct {
	onLidChange func(open bool)

	mu      sync.Mutex
	hwnd    uintptr
	notify  uintptr // HPOWERNOTIFY from RegisterPowerSettingNotification
	stopped chan struct{}
}

var (
	lidWndProcCallback     uintptr
	lidWndProcCallbackOnce sync.Once
	lidWatcherRegistry     sync.Map // hwnd(uintptr) -> *lidWatcher
)

// lidWndProc is the single shared WndProc trampoline: it looks up the
// lidWatcher registered for the target hwnd (there is normally exactly one
// per process) and dispatches WM_POWERBROADCAST to it, falling back to
// DefWindowProc for everything else.
//
// lparam is declared unsafe.Pointer (not uintptr) so windows.NewCallback's
// runtime trampoline hands it to us as a real, GC-tracked Go pointer value —
// this avoids ever writing a raw uintptr->unsafe.Pointer conversion in
// source (which `go vet`'s unsafeptr analyzer always flags, since it can't
// prove such a conversion is safe in the general case). The Win32 LPARAM is
// still passed on the wire as a machine word; only its Go-side type differs.
func lidWndProc(hwnd uintptr, message uint32, wparam uintptr, lparam unsafe.Pointer) uintptr {
	if message == wmPowerBroadcast && wparam == pbtPowerSettingChange && lparam != nil {
		if v, ok := lidWatcherRegistry.Load(hwnd); ok {
			w := v.(*lidWatcher)
			pbs := (*powerBroadcastSetting)(lparam)
			if pbs.PowerSetting == guidLidSwitchStateChange && pbs.DataLength >= 1 {
				w.onLidChange(pbs.Data[0] != 0)
			}
		}
		return 0
	}
	if message == wmDestroy {
		_, _, _ = procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, uintptr(message), wparam, uintptr(lparam))
	return r
}

// newLidWatcher creates the hidden message-only window, registers for
// GUID_LIDSWITCH_STATE_CHANGE, and starts the message pump goroutine. onLid
// is invoked (from the pump goroutine) with true=open, false=closed whenever
// Windows reports a lid transition.
func newLidWatcher(onLid func(open bool)) (*lidWatcher, error) {
	lidWndProcCallbackOnce.Do(func() {
		lidWndProcCallback = windows.NewCallback(lidWndProc)
	})

	w := &lidWatcher{onLidChange: onLid, stopped: make(chan struct{})}
	ready := make(chan error, 1)

	go func() {
		// Win32 windows are thread-affine: the thread that creates the window
		// must be the one that pumps its messages, so lock this goroutine to
		// its OS thread for its entire lifetime.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		className, _ := windows.UTF16PtrFromString("CerberusLifecycleLidWatcher")
		windowName, _ := windows.UTF16PtrFromString("cerberus-lifecycle-lid")

		wc := wndClassExW{
			WndProc:   lidWndProcCallback,
			ClassName: className,
		}
		wc.Size = uint32(unsafe.Sizeof(wc))
		// RegisterClassExW may legitimately fail with ERROR_CLASS_ALREADY_EXISTS
		// if a prior watcher in this process registered it; that's fine, we can
		// still create a window against the existing class.
		_, _, _ = procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

		hwnd, _, err := procCreateWindowExW.Call(
			0,
			uintptr(unsafe.Pointer(className)),
			uintptr(unsafe.Pointer(windowName)),
			uintptr(wsOverlapped),
			uintptr(cwUseDefault), uintptr(cwUseDefault),
			uintptr(cwUseDefault), uintptr(cwUseDefault),
			uintptr(hwndMessageOnly), // HWND_MESSAGE parent: message-only, never shown
			0, 0, 0,
		)
		if hwnd == 0 {
			ready <- err
			return
		}
		w.mu.Lock()
		w.hwnd = hwnd
		w.mu.Unlock()
		lidWatcherRegistry.Store(hwnd, w)

		notify, _, nerr := procRegisterPowerSettingNotification.Call(
			hwnd,
			uintptr(unsafe.Pointer(&guidLidSwitchStateChange)),
			uintptr(deviceNotifyWindowHandle),
		)
		if notify == 0 {
			lidWatcherRegistry.Delete(hwnd)
			_, _, _ = procDestroyWindow.Call(hwnd)
			ready <- nerr
			return
		}
		w.mu.Lock()
		w.notify = notify
		w.mu.Unlock()

		ready <- nil

		var m msg
		for {
			r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
			// GetMessage returns 0 on WM_QUIT, -1 on error; either way, stop.
			if int32(r) <= 0 {
				break
			}
			_, _, _ = procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
			_, _, _ = procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
		}

		lidWatcherRegistry.Delete(hwnd)
		close(w.stopped)
	}()

	if err := <-ready; err != nil {
		return nil, err
	}
	return w, nil
}

// close unregisters the power-setting notification and tears down the hidden
// window, waiting for the pump goroutine to exit.
func (w *lidWatcher) close() error {
	w.mu.Lock()
	notify := w.notify
	hwnd := w.hwnd
	w.mu.Unlock()

	var firstErr error
	if notify != 0 {
		if ret, _, err := procUnregisterPowerSettingNotification.Call(notify); ret == 0 && firstErr == nil {
			firstErr = err
		}
	}
	if hwnd != 0 {
		_, _, _ = procPostMessageW.Call(hwnd, uintptr(wmClose), 0, 0)
		_, _, _ = procDestroyWindow.Call(hwnd) // triggers WM_DESTROY -> PostQuitMessage in lidWndProc
	}
	<-w.stopped
	return firstErr
}
