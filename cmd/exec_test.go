package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
