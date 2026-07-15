package testdaemon

import (
	"io"
	"os/exec"
	"time"
)

// TerminateProcess kills cmd's OS process, waits up to graceful for exit, and
// closes stdout/stderr pipes so Windows releases any handle on the executable.
func TerminateProcess(cmd *exec.Cmd, stdout, stderr io.Closer, wait <-chan error, graceful time.Duration) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	if wait != nil {
		select {
		case <-wait:
		case <-time.After(graceful):
		}
	}
	if stdout != nil {
		_ = stdout.Close()
	}
	if stderr != nil {
		_ = stderr.Close()
	}
}
