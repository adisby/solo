package main

import "os"

// shutdownSignals are the events that ask the Daemon to shut down gracefully.
//
// Windows has no SIGTERM: os.Interrupt is what Go reports for a CTRL_C_EVENT or
// CTRL_BREAK_EVENT delivered by the console, which is what `solo daemon stop`
// sends.
var shutdownSignals = []os.Signal{os.Interrupt}
