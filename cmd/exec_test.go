package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/redscaresu/goldfinger/apply"
	"github.com/redscaresu/goldfinger/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecRunSuccess(t *testing.T) {
	err := execRun(context.Background(), "sh", []string{"-c", "exit 0"}, os.Environ())
	assert.NoError(t, err)
}

func TestExecRunFailure(t *testing.T) {
	err := execRun(context.Background(), "sh", []string{"-c", "exit 3"}, os.Environ())
	assert.Error(t, err)
}

func TestExecRunUnknownBinary(t *testing.T) {
	err := execRun(context.Background(), "definitely-not-a-real-binary-xyz", nil, os.Environ())
	assert.Error(t, err)
}

// TestExecRunRoutesChildStdoutToStderr locks the item-D contract: a delegate's
// stdout must land on the process's stderr, never its stdout, so goldfinger's
// own stdout (the machine-readable workspace path) can't be contaminated by
// ghorg/multi-gitter chatter. It swaps os.Stdout/os.Stderr for pipes around one
// child run (the test package runs sequentially, so the global swap is safe).
func TestExecRunRoutesChildStdoutToStderr(t *testing.T) {
	outR, outW, err := os.Pipe()
	require.NoError(t, err)
	errR, errW, err := os.Pipe()
	require.NoError(t, err)

	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	runErr := execRun(context.Background(), "sh", []string{"-c", "echo child-stdout"}, os.Environ())
	os.Stdout, os.Stderr = origOut, origErr
	require.NoError(t, outW.Close())
	require.NoError(t, errW.Close())
	require.NoError(t, runErr)

	gotOut, err := io.ReadAll(outR)
	require.NoError(t, err)
	gotErr, err := io.ReadAll(errR)
	require.NoError(t, err)

	assert.Empty(t, string(gotOut), "child stdout must not reach process stdout")
	assert.Contains(t, string(gotErr), "child-stdout", "child stdout must be routed to stderr")
}

func TestExecApplyRunDryRunCapturesAndTeesCombinedOutput(t *testing.T) {
	outR, outW, err := os.Pipe()
	require.NoError(t, err)
	errR, errW, err := os.Pipe()
	require.NoError(t, err)

	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	got, runErr := execApplyRun(context.Background(), "sh", []string{"-c", "echo child-stdout; echo child-stderr >&2", "--dry-run"}, os.Environ())
	os.Stdout, os.Stderr = origOut, origErr
	require.NoError(t, outW.Close())
	require.NoError(t, errW.Close())
	require.NoError(t, runErr)

	gotOut, err := io.ReadAll(outR)
	require.NoError(t, err)
	gotErr, err := io.ReadAll(errR)
	require.NoError(t, err)

	assert.Empty(t, string(gotOut), "apply delegate stdout must not reach process stdout")
	assert.Contains(t, string(got), "child-stdout")
	assert.Contains(t, string(got), "child-stderr")
	assert.Contains(t, string(gotErr), "child-stdout", "dry-run stdout should still tee to stderr")
	assert.Contains(t, string(gotErr), "child-stderr", "dry-run stderr should still tee to stderr")
}

func TestExecApplyRunQuietCapturesWithoutTeeing(t *testing.T) {
	outR, outW, err := os.Pipe()
	require.NoError(t, err)
	errR, errW, err := os.Pipe()
	require.NoError(t, err)

	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	got, runErr := execApplyRunQuiet(context.Background(), "sh", []string{"-c", "echo child-stdout; echo child-stderr >&2", "--dry-run"}, os.Environ())
	os.Stdout, os.Stderr = origOut, origErr
	require.NoError(t, outW.Close())
	require.NoError(t, errW.Close())
	require.NoError(t, runErr)

	gotOut, err := io.ReadAll(outR)
	require.NoError(t, err)
	gotErr, err := io.ReadAll(errR)
	require.NoError(t, err)

	assert.Empty(t, string(gotOut), "quiet apply delegate stdout must not reach process stdout")
	assert.Empty(t, string(gotErr), "quiet apply delegate output must not tee to stderr")
	assert.Contains(t, string(got), "child-stdout")
	assert.Contains(t, string(got), "child-stderr")
}

func TestExecApplyRunLiveStreamsWithoutCapture(t *testing.T) {
	got, err := execApplyRun(context.Background(), "sh", []string{"-c", "exit 0"}, os.Environ())
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestExecApplyRunDryRunBoundsTheRetainedOutput(t *testing.T) {
	// A chatty change command times a large fleet is how the captured buffer used to
	// grow without limit. Retention is now the last maxCapturedOutput bytes, which
	// is the half that matters: multi-gitter writes its repo-counter block last, so
	// the tail is what the digest is parsed from. The sentinel stands in for that
	// block.
	lines := (maxCapturedOutput / 8) + 50_000 // ~8 bytes/line, comfortably over the limit
	script := "yes 0123456 | head -n " + strconv.Itoa(lines) + "; echo tail-sentinel"

	got, err := execApplyRunQuiet(context.Background(), "sh", []string{"-c", script, "--dry-run"}, os.Environ())
	require.NoError(t, err)

	assert.Contains(t, string(got), "tail-sentinel", "the tail — where the result block lands — must survive")
	assert.Less(t, len(got), maxCapturedOutput+512, "retention must be bounded, marker aside")
	assert.True(t, strings.HasPrefix(string(got), "[goldfinger] earlier output dropped"),
		"a truncated capture must say so: apply writes this buffer to a file it labels the full run output")
}

func TestExecApplyRunDryRunUntruncatedOutputCarriesNoMarker(t *testing.T) {
	got, err := execApplyRunQuiet(context.Background(), "sh", []string{"-c", "echo short", "--dry-run"}, os.Environ())
	require.NoError(t, err)
	assert.Equal(t, "short\n", string(got), "an output that fits is returned verbatim, with no marker")
}

// TestTruncationNeverManufacturesAResultSectionHeader covers the sharper half of
// the same hazard: the fragment truncation leaves of the line it cut through. Here
// the cut lands inside "Repositories with a successful run:", leaving
// "positories with a successful run:" — a line the parser reads as a section
// header because it ends in ":", though no such line was ever written. octo/a would
// bucket under it as an error with that fragment as the reason, and the REAL header
// further down would set the parser's recognised-format flag, so the digest would
// look parseable and the misreport would pass unnoticed. Discarding the fragment is
// what keeps octo/a honestly unknown.
func TestTruncationNeverManufacturesAResultSectionHeader(t *testing.T) {
	repos := []models.Repo{{Owner: "octo", Name: "a"}, {Owner: "octo", Name: "b"}}
	tail := "positories with a successful run:\n  octo/a\nNo data was changed:\n  octo/b\n"
	buf := &boundedBuffer{limit: len(tail)}
	_, _ = buf.Write([]byte("Re" + tail))
	require.True(t, buf.truncated, "the cut must land inside the header for this to test anything")

	digest := apply.SummarizeDryRunOutput(repos, capturedOutput(buf))

	assert.Equal(t, apply.DryRunUnknown, digest.Repos[0].Status,
		"a repo under a header that truncation invented is unknown, never confidently bucketed")
	assert.Zero(t, digest.Errored, "a line fragment must not become an error section")
	assert.Equal(t, apply.DryRunNoChange, digest.Repos[1].Status,
		"the intact section after the fragment must still parse — dropping the fragment must not cost real data")
}

// TestTruncationMarkerCannotBecomeAResultSectionHeader is the reason the marker
// ends in "." rather than ":". Once the cut fragment is discarded the retained tail
// can legitimately begin with a section's indented repo lines — its header having
// BEEN that fragment — so the marker is then the first header-shaped line the parser
// meets. multi-gitter's headers are exactly "lines ending in a colon", so a marker
// ending in ":" would become the bucket for those repos and label them errored with
// the marker text as the reason. Leaving them "unknown" is the truth.
func TestTruncationMarkerCannotBecomeAResultSectionHeader(t *testing.T) {
	repos := []models.Repo{{Owner: "octo", Name: "repo"}}
	// What truncation leaves of "No data was changed:\n  octo/repo\n" when the cut
	// lands three bytes into the header: the fragment goes, the repo line stays.
	tail := "ed:\n  octo/repo\n"
	buf := &boundedBuffer{limit: len(tail)}
	_, _ = buf.Write([]byte("No data was chang" + tail))
	require.True(t, buf.truncated, "the header must have been cut for this to test anything")

	digest := apply.SummarizeDryRunOutput(repos, capturedOutput(buf))

	assert.Equal(t, apply.DryRunUnknown, digest.Repos[0].Status,
		"a repo whose section header was truncated away is unknown, never bucketed under the marker")
	assert.Zero(t, digest.Errored, "the marker must not be read as an error section header")
}

// TestNewGhorgLogIsOwnerOnly locks the 0600 perm on the captured mirror output
// log — it can carry ghorg clone errors that mention repo names, so it must not
// be world-readable, matching apply's captured-output posture.
func TestNewGhorgLogIsOwnerOnly(t *testing.T) {
	f, err := newGhorgLog()
	require.NoError(t, err)
	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()

	info, err := os.Stat(f.Name())
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "the captured mirror output log must be owner-only")
}

func TestHasArg(t *testing.T) {
	assert.True(t, hasArg([]string{"run", "--dry-run"}, "--dry-run"))
	assert.False(t, hasArg([]string{"run", "--dry-run=false"}, "--dry-run"))
	assert.False(t, hasArg([]string{"run", "--not-dry-run"}, "--dry-run"))
}

func TestRequireToolPresent(t *testing.T) {
	// sh is always on PATH on the platforms goldfinger targets.
	require.NoError(t, requireTool("sh", "install a POSIX shell"))
}

func TestRequireToolInGoBinHintsPath(t *testing.T) {
	// A tool that's installed under GOBIN but not on PATH should get a
	// PATH-export hint, not a reinstall instruction.
	gobin := t.TempDir()
	tool := "goldfinger-fake-tool"
	require.NoError(t, os.WriteFile(filepath.Join(gobin, tool), []byte("#!/bin/sh\n"), 0o755))
	t.Setenv("GOBIN", gobin)
	t.Setenv("PATH", "") // ensure LookPath can't find it

	err := requireTool(tool, "https://example.test/install")
	require.Error(t, err)
	assert.Contains(t, err.Error(), gobin)
	assert.Contains(t, err.Error(), "not on PATH")
	assert.NotContains(t, err.Error(), "https://example.test/install")
}

func TestRequireToolMissingEverywhereHintsInstall(t *testing.T) {
	t.Setenv("GOBIN", t.TempDir()) // empty dir
	t.Setenv("GOPATH", t.TempDir())
	t.Setenv("PATH", "")

	err := requireTool("goldfinger-absent-tool", "https://example.test/install")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "https://example.test/install")
}
