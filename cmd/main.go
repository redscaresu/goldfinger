package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/redscaresu/goldfinger/apply"
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

// interruptContext returns a root context cancelled by the first SIGINT or
// SIGTERM, plus a function that restores default signal handling.
//
// It is one half of a pair and only works as one. Ctrl-C used to reach the
// delegates for free — ghorg and multi-gitter shared goldfinger's process group,
// so the terminal signalled them too. They now run in their own group
// (setDelegateLifecycle), which means the terminal no longer reaches them and this
// context is the ONLY thing that stops a running mirror or apply. Neither change is
// safe alone: process groups without this would orphan a delegate that kept cloning
// after Ctrl-C, and this without process groups would report a cancellation while
// the delegate's git children carried on.
//
// Handling is restored BEFORE the context is cancelled, so a second Ctrl-C kills
// the process outright. That escape hatch is the reason for hand-rolling rather
// than calling signal.NotifyContext: NotifyContext keeps the signal registered
// after it fires, so every later Ctrl-C is swallowed and an operator whose delegate
// is ignoring the kill has no way out short of another terminal.
func interruptContext(parent context.Context) (context.Context, func()) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	restore := func() { signal.Stop(sigs) }
	return cancelOnSignal(parent, sigs, restore), restore
}

// cancelOnSignal is interruptContext's testable core: the ordering (restore, then
// cancel) is the property worth pinning, so it takes the channel and the restore
// hook rather than installing them itself.
func cancelOnSignal(parent context.Context, sigs <-chan os.Signal, restore func()) context.Context {
	ctx, cancel := context.WithCancel(parent)
	go func() {
		select {
		case <-sigs:
			restore()
			cancel()
		case <-ctx.Done():
			cancel()
		}
	}()
	return ctx
}

// interruptError collapses an interrupted run's failure into one honest word. A
// cancelled delegate surfaces as "signal: killed" wrapped in whichever step was
// running, which reads like goldfinger broke rather than like the operator stopped
// it. The exit code stays 2: an interrupted run did not complete, so it is a
// failure, not a domain signal (1, as `check` uses for drift).
//
// An error already tagged apply.ErrInterrupted is kept verbatim: it came from code
// that noticed the cancellation itself and had more to say than the fact of it —
// a batched apply names how many batches already ran, and on a real run those PRs
// are open. Losing that to a generic word would be the more expensive silence.
func interruptError(ctx context.Context, err error) error {
	if err == nil || ctx.Err() == nil {
		return err
	}
	if errors.Is(err, apply.ErrInterrupted) {
		return err
	}
	return errors.New("interrupted")
}

func main() {
	ctx, restoreSignals := interruptContext(context.Background())
	root := newRootCmd()
	cmd, err := root.ExecuteContextC(ctx)
	// os.Exit skips defers, so restore explicitly before reporting.
	restoreSignals()
	os.Exit(reportExit(resolveQuiet(root, cmd, os.Args[1:]), interruptError(ctx, err), os.Stderr))
}
