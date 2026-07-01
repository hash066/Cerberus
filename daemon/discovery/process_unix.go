//go:build !windows

package discovery

import "syscall"

// isRunning sends signal 0, which performs no action but still fails with
// ESRCH if the process does not exist -- the standard POSIX liveness probe.
func isRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
