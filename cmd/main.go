package main

import (
	"errors"
	"fmt"
	"os"
)

// exitError lets a command choose the process exit code. It carries an optional
// wrapped error; a nil/empty message (as used for detected drift) exits with the
// code but prints nothing, because the command already wrote its report to
// stdout.
type exitError struct {
	code int
	err  error
}

func (e exitError) Error() string {
	if e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e exitError) Unwrap() error { return e.err }

// exitCode maps a command error to a process exit code: 0 for success, an
// exitError's own code when it carries one (e.g. 1 for drift), and 2 for any
// other error.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	return 2
}

func main() {
	err := newRootCmd().Execute()
	// The root command sets SilenceErrors, so genuine failures are printed here.
	// A drift exitError carries no message and so exits non-zero silently.
	if err != nil && err.Error() != "" {
		fmt.Fprintln(os.Stderr, "Error:", err)
	}
	os.Exit(exitCode(err))
}
