package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveWorkspaceDefault(t *testing.T) {
	ws, err := resolveWorkspace("")
	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(ws))
	assert.True(t, strings.HasSuffix(ws, "goldfinger"))
}

func TestResolveWorkspaceRelativeBecomesAbsolute(t *testing.T) {
	ws, err := resolveWorkspace("some/dir")
	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(ws))
	assert.True(t, strings.HasSuffix(ws, filepath.Join("some", "dir")))
}

func TestRequireToolMissing(t *testing.T) {
	err := requireTool("definitely-not-a-real-tool-xyz", "brew install foo")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "brew install foo")
}

func TestMirrorCmdMissingToken(t *testing.T) {
	// The token guard returns before any tool/network use, so this is
	// deterministic regardless of whether ghorg is installed.
	_, err := executeCmd(t, "", "mirror")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "GOLD_FINGER_PAT")
}
