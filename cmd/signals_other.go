//go:build !unix

package main

import (
	"os"
	"syscall"
)

// interruptSignals are the signals that cancel the root context. There is no
// SIGHUP/SIGQUIT to catch off unix; see signals_unix.go.
var interruptSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}
