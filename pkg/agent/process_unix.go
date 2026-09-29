//go:build !windows

package agent

import (
	"os"
	"syscall"
)

// isProcessAlive reports whether the given PID still refers to a running
// process. Signal 0 is defined by POSIX as an existence probe that does not
// actually deliver a signal.
func isProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
