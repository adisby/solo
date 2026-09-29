//go:build !windows

package main

import (
	"os"
	"syscall"
)

// shutdownSignals are the signals that ask the Daemon to shut down gracefully.
var shutdownSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM}
