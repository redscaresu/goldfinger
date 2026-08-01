package main

import (
	"context"
	"os"
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
