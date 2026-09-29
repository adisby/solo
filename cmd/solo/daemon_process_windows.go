package main

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

const (
	// createNewProcessGroup gives the managed Daemon its own console process
	// group. It is required for GenerateConsoleCtrlEvent(CTRL_BREAK_EVENT) to
	// address the Daemon and nothing else, and it is inherited by the agent
	// subprocesses the Daemon spawns.
	createNewProcessGroup = 0x00000200
	// createNoWindow keeps the Daemon and its console-less subprocesses from
	// flashing a console window on the user's desktop.
	createNoWindow = 0x08000000
)

// configureDetachedProcess starts the managed Daemon hidden and in its own
// process group so it survives the CLI that launched it.
//
// DETACHED_PROCESS is deliberately NOT used: a fully detached child has no
// console, which makes GenerateConsoleCtrlEvent fail and would remove the only
// graceful stop path on Windows.
func configureDetachedProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNewProcessGroup | createNoWindow,
	}
}

// requestDaemonStop asks the Daemon to shut down gracefully.
//
// The Daemon registers shutdownSignals (os.Interrupt on Windows), and
// GenerateConsoleCtrlEvent delivers CTRL_BREAK_EVENT to the target process
// group as exactly that interrupt. When it is delivered, the Daemon runs its
// normal shutdown path — reaping provider subprocesses, unregistering from the
// Server, and releasing the machine lock.
//
// Windows has no portable graceful-stop primitive, so this is best effort: a
// Daemon started with CREATE_NO_WINDOW owns a separate console and may not be
// reachable by a console event. The fallbacks are a non-forced taskkill (which
// needs a window to post WM_CLOSE to) and then a forced kill, so that
// `solo daemon stop` actually stops the Daemon instead of failing outright.
// stopManagedDaemonProfile still verifies the outcome and reports a Daemon that
// survived all three attempts.
func requestDaemonStop(process *os.Process) error {
	if err := windowsGenerateConsoleCtrlEvent(process.Pid); err == nil {
		return nil
	}
	if err := exec.Command("taskkill", "/PID", strconv.Itoa(process.Pid)).Run(); err == nil {
		return nil
	}
	// Last resort: a forced kill skips the Daemon's shutdown path, so the next
	// start clears the leftover lock file through the PID-liveness check.
	return exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(process.Pid)).Run()
}

// windowsGenerateConsoleCtrlEvent sends CTRL_BREAK_EVENT to the process group
// led by pid.
func windowsGenerateConsoleCtrlEvent(pid int) error {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	proc := kernel32.NewProc("GenerateConsoleCtrlEvent")
	result, _, err := proc.Call(uintptr(syscall.CTRL_BREAK_EVENT), uintptr(uint32(pid)))
	if result == 0 {
		return err
	}
	return nil
}

// daemonExecutableName returns the platform-specific file name of a Solo
// binary.
func daemonExecutableName(base string) string {
	return base + ".exe"
}
