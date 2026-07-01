//go:build windows

package discovery

import "golang.org/x/sys/windows"

// isRunning opens the process with only the query right; a stale/exited PID
// (possibly reused by an unrelated process, which OpenProcess would still
// happily open) is distinguished by checking the exit code -- a live process
// reports STILL_ACTIVE.
func isRunning(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)

	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259 // STILL_ACTIVE
	return code == stillActive
}
