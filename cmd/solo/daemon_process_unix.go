//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// configureDetachedProcess starts the managed Daemon in its own session so it
// keeps running after the CLI that launched it returns.
func configureDetachedProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// requestDaemonStop asks the Daemon to shut down gracefully. The managed
// Daemon does not hold a controlling terminal, so SIGTERM is the portable
// request on POSIX systems.
func requestDaemonStop(process *os.Process) error {
	return process.Signal(syscall.SIGTERM)
}

// daemonExecutableName returns the platform-specific file name of a Solo
// binary.
func daemonExecutableName(base string) string {
	return base
}
