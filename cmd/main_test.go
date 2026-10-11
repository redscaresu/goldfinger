package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/redscaresu/goldfinger/apply"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCancelOnSignalRestoresHandlingBeforeCancelling pins the ordering, which is
// the whole reason this is hand-rolled instead of signal.NotifyContext. The
// delegates now run in their own process group, so the terminal no longer reaches
// them and this context is the only thing that stops a running mirror; if the
// delegate is wedged, the operator's next move is a second Ctrl-C, and that only
// works if default handling was restored first. NotifyContext leaves the signal
// registered, swallowing every later one.
func TestCancelOnSignalRestoresHandlingBeforeCancelling(t *testing.T) {
	sigs := make(chan os.Signal, 1)
	// The hook reports what the context looked like AT restore-time, which is what
	// makes this an ordering assertion rather than a liveness one: merely waiting for
	// both to happen would pass just as happily on a `cancel(); restore()`
	// implementation, and that is precisely the version with the swallowed Ctrl-C
	// window. Reading ctx from the hook is ordered, not racy — the assignment below
	// happens-before the send on sigs, which happens-before the receive that runs it.
	var ctx context.Context
	observed := make(chan error, 1)
	ctx = cancelOnSignal(context.Background(), sigs, func() { observed <- ctx.Err() })

	require.NoError(t, ctx.Err(), "the context must stay live until a signal arrives")
	sigs <- syscall.SIGINT

	select {
	case err := <-observed:
		assert.NoError(t, err,
			"the context was already cancelled when handling was restored — the run starts unwinding "+
				"during a window where a second Ctrl-C is still swallowed")
	case <-time.After(5 * time.Second):
		t.Fatal("default signal handling was never restored — a second Ctrl-C would be swallowed")
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the signal did not cancel the root context")
	}
	assert.Equal(t, context.Canceled, ctx.Err())
}

// TestInterruptError locks the message an interrupted run reports. A cancelled
// delegate surfaces as "signal: killed" wrapped in whichever step was running,
// which reads as a goldfinger failure rather than as the operator stopping it.
func TestInterruptError(t *testing.T) {
	live, cancel := context.WithCancel(context.Background())
	defer cancel()
	killed := errors.New("multi-gitter run: signal: killed")

	assert.Equal(t, killed, interruptError(live, killed), "an uncancelled context leaves the real error alone")
	assert.NoError(t, interruptError(live, nil))

	cancelled, stop := context.WithCancel(context.Background())
	stop()
	assert.NoError(t, interruptError(cancelled, nil), "a run that finished despite the signal is still a success")
	err := interruptError(cancelled, killed)
	require.Error(t, err)
	assert.Equal(t, "interrupted", err.Error())
	assert.Equal(t, 2, exitCode(err), "an interrupted run did not complete: a failure, not a domain signal")

	// The exception: an error that already identified itself as an interruption
	// survives whole. A batched apply reports how far it got, and on a real run
	// those PRs exist on GitHub — collapsing that to one word would hide the only
	// thing the operator now has to act on.
	progress := fmt.Errorf("%w after batch 2/5 — those PRs are already open", apply.ErrInterrupted)
	assert.Equal(t, progress, interruptError(cancelled, progress))
	assert.Contains(t, interruptError(cancelled, progress).Error(), "batch 2/5")
}

// TestExitErrorMessage locks the other half of the exit-code contract (the code
// mapping itself is covered by TestExitCode): a code-only exitError — as `check`
// uses for drift — prints nothing, because its report already went to stdout,
// while an exitError wrapping a real error surfaces that message on stderr.
func TestExitErrorMessage(t *testing.T) {
	assert.Empty(t, exitError{code: 1}.Error())
	assert.Equal(t, "bad flag", exitError{code: 2, err: errors.New("bad flag")}.Error())
}

// TestReportExit locks WS5's failure contract: a genuine error is exactly one
// line (never a stack dump), a drift/fail signal stays silent, and --quiet turns
// the failure into the compact errorReport JSON — each with the right exit code.
func TestReportExit(t *testing.T) {
	t.Run("success prints nothing and exits 0", func(t *testing.T) {
		var buf bytes.Buffer
		assert.Equal(t, 0, reportExit(false, nil, &buf))
		assert.Empty(t, buf.String())
	})

	t.Run("human error is a single Error: line, exit 2", func(t *testing.T) {
		var buf bytes.Buffer
		code := reportExit(false, errors.New("no token found"), &buf)
		assert.Equal(t, 2, code)
		assert.Equal(t, "Error: no token found\n", buf.String())
	})

	t.Run("multi-line error is collapsed to one line — never a stack dump", func(t *testing.T) {
		var buf bytes.Buffer
		reportExit(false, errors.New("mirror failed:\n  ghorg: exit 1\n  repo x\n"), &buf)
		out := buf.String()
		assert.Equal(t, 1, strings.Count(out, "\n"), "collapsed to a single trailing newline")
		assert.NotContains(t, out, "\n  ")
		assert.Contains(t, out, "mirror failed: ghorg: exit 1 repo x")
	})

	t.Run("drift/fail exitError is silent, code carries the signal", func(t *testing.T) {
		var buf bytes.Buffer
		assert.Equal(t, 1, reportExit(false, exitError{code: 1}, &buf))
		assert.Empty(t, buf.String(), "a code-only exitError prints nothing")
		// Silent under quiet too.
		buf.Reset()
		assert.Equal(t, 1, reportExit(true, exitError{code: 1}, &buf))
		assert.Empty(t, buf.String())
	})

	t.Run("quiet error is compact errorReport JSON on stderr, exit 2", func(t *testing.T) {
		var buf bytes.Buffer
		code := reportExit(true, errors.New("verifying token: unauthorized"), &buf)
		assert.Equal(t, 2, code)
		assert.Equal(t, 1, strings.Count(buf.String(), "\n"), "single-line JSON")
		assert.NotContains(t, buf.String(), "\n  ", "compact, not indented")

		var rep errorReport
		require.NoError(t, json.Unmarshal(buf.Bytes(), &rep))
		assert.Equal(t, errorReportVersion, rep.Version)
		assert.Equal(t, "verifying token: unauthorized", rep.Error)
		assert.Equal(t, 2, rep.ExitCode)
	})

	t.Run("quiet preserves a wrapped exitError's own code", func(t *testing.T) {
		var buf bytes.Buffer
		code := reportExit(true, exitError{code: 2, err: errors.New("bad flag")}, &buf)
		assert.Equal(t, 2, code)
		var rep errorReport
		require.NoError(t, json.Unmarshal(buf.Bytes(), &rep))
		assert.Equal(t, 2, rep.ExitCode)
		assert.Equal(t, "bad flag", rep.Error)
	})
}

// TestResolveQuiet locks the failure-path quiet detection: a parse error that
// fails before a subcommand resolves (an unknown command) leaves the executed
// command's flags unparsed, so quiet must be recovered from the raw args or the
// machine would get a human "Error:" line for exactly the malformed invocations
// an agent is most likely to trip. The resolved-command flag is still trusted
// first.
func TestResolveQuiet(t *testing.T) {
	// The fallback branch: cmd is the unparsed root (as ExecuteC returns it when
	// an unknown command fails before a subcommand resolves), so quiet must be
	// recovered from the raw args.
	fallback := []struct {
		name string
		args []string
		want bool
	}{
		{"no quiet, unknown command", []string{"boguscmd"}, false},
		{"--quiet before unknown command", []string{"--quiet", "boguscmd"}, true},
		{"-q after unknown command", []string{"boguscmd", "-q"}, true},
		{"--quiet with unknown flag", []string{"--quiet", "--nope"}, true},
		{"no args", nil, false},
	}
	for _, tt := range fallback {
		t.Run(tt.name, func(t *testing.T) {
			// A fresh root each time: Parse mutates flag state, and main builds a
			// new root per process, so the helper only ever sees a pristine one.
			root := newRootCmd()
			assert.Equal(t, tt.want, resolveQuiet(root, root, tt.args))
		})
	}

	t.Run("trusts the resolved command's parsed flag over args", func(t *testing.T) {
		// When a subcommand resolves, its flag is authoritative — even if the raw
		// args wouldn't re-parse to the same value.
		root := newRootCmd()
		cmd := newRootCmd()
		// ParseFlags merges the persistent flag into Flags() just as execution
		// does, so quietRequested(cmd) reads a genuinely-parsed value.
		require.NoError(t, cmd.ParseFlags([]string{"--quiet"}))
		require.True(t, quietRequested(cmd))
		// args here have no quiet token, proving the resolved flag wins over them.
		assert.True(t, resolveQuiet(root, cmd, []string{"boguscmd"}))
	})
}
