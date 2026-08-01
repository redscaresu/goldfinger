package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/redscaresu/goldfinger/mirror"
	"github.com/redscaresu/goldfinger/models"
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

func TestRunMirror(t *testing.T) {
	sel := models.Selection{Owner: "redscaresu", OwnerType: models.OwnerUser, Repos: []models.Repo{{Owner: "redscaresu", Name: "a"}}}

	t.Run("success frames and delegates", func(t *testing.T) {
		var gotArgs []string
		run := func(_ context.Context, _ string, args, _ []string) error { gotArgs = args; return nil }
		var errOut bytes.Buffer
		err := runMirror(context.Background(), run, sel, "/tmp/ws", "tok", mirror.Options{}, &errOut)
		require.NoError(t, err)
		assert.Contains(t, errOut.String(), "Mirroring 1 repo(s)")
		assert.Contains(t, errOut.String(), "mirror complete")
		assert.Contains(t, gotArgs, "--path=/tmp/ws")
	})

	t.Run("propagates delegate error", func(t *testing.T) {
		run := func(_ context.Context, _ string, _, _ []string) error { return errors.New("ghorg blew up") }
		err := runMirror(context.Background(), run, sel, "/tmp/ws", "tok", mirror.Options{}, &bytes.Buffer{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ghorg blew up")
	})
}

func TestMirrorCmdMissingToken(t *testing.T) {
	// The token guard returns before any tool/network use, so this is
	// deterministic regardless of whether ghorg is installed.
	_, err := executeCmd(t, "", "mirror")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "GOLD_FINGER_PAT")
}
