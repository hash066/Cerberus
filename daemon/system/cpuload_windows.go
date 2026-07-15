//go:build windows

package system

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// GetSystemTimes is not wrapped by this pinned golang.org/x/sys/windows, so we
// bind it from kernel32 directly (the same LazyDLL pattern daemon/audio uses for
// WASAPI). It returns cumulative idle/kernel/user CPU time across all logical
// processors as FILETIMEs (100 ns ticks).
var (
	cpuLoadKernel32       = windows.NewLazySystemDLL("kernel32.dll")
	cpuLoadGetSystemTimes = cpuLoadKernel32.NewProc("GetSystemTimes")
)

// readCPUTimes reads Windows' cumulative system CPU times via GetSystemTimes.
// On Windows the reported "kernel" time INCLUDES idle time, so total elapsed
// CPU time across all logical processors is kernel+user and the idle portion is
// the separate idle counter — busy = (kernel+user) − idle. Units are 100 ns
// ticks; the sampler only differences them, so the absolute unit is irrelevant.
func readCPUTimes() (idle, total uint64, ok bool) {
	var idleT, kernelT, userT windows.Filetime
	r, _, _ := cpuLoadGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idleT)),
		uintptr(unsafe.Pointer(&kernelT)),
		uintptr(unsafe.Pointer(&userT)),
	)
	if r == 0 {
		return 0, 0, false
	}
	i := filetimeTicks(idleT)
	k := filetimeTicks(kernelT)
	u := filetimeTicks(userT)
	return i, k + u, true
}

func filetimeTicks(ft windows.Filetime) uint64 {
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}
