//go:build unix

package main

import (
	"os"
	"syscall"
)

// interruptSignals are the signals that cancel the root context. SIGHUP is the
// one that matters most beyond Ctrl-C: closing the terminal (or an ssh drop)
// sends it, and the delegates run in their own process group, so the terminal's
// hangup no longer reaches them. Uncaught, it would kill goldfinger outright and
// leave ghorg or multi-gitter running orphaned; caught, it cancels the context
// and the process groups are killed like any other interrupt. SIGQUIT is the
// same story for Ctrl-\.
var interruptSignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT}
