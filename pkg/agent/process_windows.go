package agent

import (
	"errors"
	"syscall"
	"unsafe"
)

const (
	// processQueryLimitedInformation is the least privilege right that still
	// allows querying process status. Using it instead of the
	// PROCESS_ALL_ACCESS that os.FindProcess requests lets the liveness probe
	// inspect processes owned by other users.
	processQueryLimitedInformation = 0x1000
	// stillActive is the exit code Windows reports for a process that has not
	// terminated.
	stillActive = 259
)

// isProcessAlive reports whether the given PID still refers to a running
// process.
//
// Windows has no signal-0 probe: os.Process.Signal is a console-control
// operation that fails outright for a process that owns no console, so the
// previous best-effort check could misreport a live Daemon as dead (letting a
// second Daemon take the machine lock) and could deliver an unintended
// CTRL_C_EVENT. Query the process object instead.
//
// Like the POSIX probe, this cannot distinguish a live process from a recycled
// PID. A lock file also compares the recorded token, so a recycled PID can only
// produce a false "still running" verdict, never a stolen lock.
func isProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}

	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	handle, _, err := kernel32.NewProc("OpenProcess").Call(
		uintptr(processQueryLimitedInformation),
		uintptr(0), // do not inherit the handle
		uintptr(uint32(pid)),
	)
	if handle == 0 {
		// Access denied still means the process object exists.
		return errors.Is(err, syscall.ERROR_ACCESS_DENIED)
	}
	defer func() {
		_, _, _ = kernel32.NewProc("CloseHandle").Call(handle)
	}()

	var exitCode uint32
	result, _, _ := kernel32.NewProc("GetExitCodeProcess").Call(
		handle,
		uintptr(unsafe.Pointer(&exitCode)),
	)
	if result == 0 {
		return false
	}
	return exitCode == stillActive
}
