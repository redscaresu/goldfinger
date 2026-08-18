package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
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

// errorReport is the machine-mode failure surface: under --quiet a genuine
// failure is emitted to stderr as this compact object instead of the human
// "Error: <msg>" line, so an agent parses one JSON value plus the exit code
// rather than scraping prose. It is versioned like every other payload and
// pinned to the schema by the golden + reflection test (schema key "error").
type errorReport struct {
	Version  int    `json:"version"`
	Error    string `json:"error"`
	ExitCode int    `json:"exitCode"`
}

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

// reportExit renders a genuine failure and returns the process exit code. It is
// the single place a fatal error surfaces (the root sets SilenceErrors), so the
// failure contract holds in one spot: a drift/fail exitError carries no message
// and exits non-zero silently (its report already went to stdout); a real error
// is always collapsed to exactly one line — never a stack dump — and, under
// --quiet, emitted as the compact errorReport JSON for cheap machine parsing.
func reportExit(quiet bool, err error, errOut io.Writer) int {
	code := exitCode(err)
	if err == nil {
		return code
	}
	// A code-only exitError (drift/fail) prints nothing: the exit code is the
	// whole signal and the report already went to stdout.
	msg := err.Error()
	if msg == "" {
		return code
	}
	// strings.Fields collapses every run of whitespace — including the newlines a
	// wrapped child-tool error can carry — into single spaces, guaranteeing a
	// single parseable line.
	msg = strings.Join(strings.Fields(msg), " ")
	if quiet {
		// Best-effort: a broken stderr cannot change the exit code the process
		// must return, so a write failure here is deliberately dropped.
		_ = emitJSON(errOut, errorReport{Version: errorReportVersion, Error: msg, ExitCode: code}, true)
	} else {
		fmt.Fprintln(errOut, "Error:", msg)
	}
	return code
}

// resolveQuiet reports whether --quiet/-q was requested. It trusts the resolved
// command's parsed flag first; but when parsing fails before a subcommand
// resolves (e.g. an unknown command), ExecuteC returns the root with its flags
// never populated, so it falls back to re-parsing just the persistent flags over
// the raw args — tolerating the unknown flags and positionals that tripped the
// real parse — so a machine still gets the compact errorReport for every failure
// path, not only those that fail after a command resolves.
func resolveQuiet(root, cmd *cobra.Command, args []string) bool {
	if quietRequested(cmd) {
		return true
	}
	pf := root.PersistentFlags()
	pf.ParseErrorsAllowlist.UnknownFlags = true
	pf.SetOutput(io.Discard)
	_ = pf.Parse(args)
	quiet, _ := pf.GetBool(quietFlagName)
	return quiet
}

func main() {
	root := newRootCmd()
	cmd, err := root.ExecuteC()
	os.Exit(reportExit(resolveQuiet(root, cmd, os.Args[1:]), err, os.Stderr))
}
