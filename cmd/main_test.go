package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
