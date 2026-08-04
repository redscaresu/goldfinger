package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redscaresu/goldfinger/mirror"
	"github.com/redscaresu/goldfinger/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveWorkspaceDefault(t *testing.T) {
	ws, err := resolveWorkspace("", "", "")
	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(ws))
	assert.True(t, strings.HasSuffix(ws, "goldfinger"))
}

func TestResolveWorkspaceRelativeBecomesAbsolute(t *testing.T) {
	ws, err := resolveWorkspace("some/dir", "", "")
	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(ws))
	assert.True(t, strings.HasSuffix(ws, filepath.Join("some", "dir")))
}

func TestResolveWorkspacePurposeIsTimestamped(t *testing.T) {
	orig := nowFunc
	nowFunc = func() time.Time { return time.Date(2026, 8, 4, 13, 20, 45, 123000000, time.UTC) }
	defer func() { nowFunc = orig }()

	ws, err := resolveWorkspace("", "keyv-cve", "")
	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(ws))
	// goldfinger stamps the time to the millisecond; the operator supplied only
	// the purpose, so each run gets its own pristine dir.
	assert.True(t, strings.HasSuffix(ws, filepath.Join("goldfinger", "keyv-cve-2026-08-04-132045.123")),
		"want ~/goldfinger/keyv-cve-2026-08-04-132045.123, got %s", ws)
}

func TestResolveWorkspacePurposeFoldsInBranch(t *testing.T) {
	orig := nowFunc
	nowFunc = func() time.Time { return time.Date(2026, 8, 4, 13, 20, 45, 123000000, time.UTC) }
	defer func() { nowFunc = orig }()

	// A branch with a slash must fold into a single safe path segment.
	ws, err := resolveWorkspace("", "keyv-cve", "feature/x")
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(ws, filepath.Join("goldfinger", "keyv-cve-feature-x-2026-08-04-132045.123")),
		"want ~/goldfinger/keyv-cve-feature-x-2026-08-04-132045.123, got %s", ws)
}

func TestResolveWorkspacePurposeAndWorkspaceMutuallyExclusive(t *testing.T) {
	_, err := resolveWorkspace("/some/ws", "keyv-cve", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

func TestResolveWorkspaceRejectsUnsafePurpose(t *testing.T) {
	for _, p := range []string{"../etc", "a/b", "has space", "..", "with\\slash"} {
		_, err := resolveWorkspace("", p, "")
		require.Error(t, err, "purpose %q should be rejected", p)
		assert.Contains(t, err.Error(), "--purpose")
	}
}

func TestRequireToolMissing(t *testing.T) {
	err := requireTool("definitely-not-a-real-tool-xyz", "brew install foo")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "brew install foo")
}

func TestRunMirror(t *testing.T) {
	sel := models.Selection{Owner: "redscaresu", OwnerType: models.OwnerUser, Repos: []models.Repo{{Owner: "redscaresu", Name: "a"}}}

	t.Run("prints workspace path to stdout, banners to stderr", func(t *testing.T) {
		var gotArgs []string
		run := func(_ context.Context, _ string, args, _ []string) error { gotArgs = args; return nil }
		var out, errOut bytes.Buffer
		err := runMirror(context.Background(), run, sel, "/tmp/ws", "tok", mirror.Options{}, &out, &errOut)
		require.NoError(t, err)
		// stdout is exactly the bare absolute path (plus newline) so a script can
		// capture it — nothing else leaks onto stdout.
		assert.Equal(t, "/tmp/ws\n", out.String())
		// Human banners stay on stderr, keeping stdout parseable.
		assert.Contains(t, errOut.String(), "Mirroring 1 repo(s)")
		assert.Contains(t, errOut.String(), "mirror complete")
		assert.NotContains(t, errOut.String(), "\n/tmp/ws\n")
		assert.Contains(t, gotArgs, "--path=/tmp/ws")
	})

	t.Run("propagates delegate error", func(t *testing.T) {
		run := func(_ context.Context, _ string, _, _ []string) error { return errors.New("ghorg blew up") }
		err := runMirror(context.Background(), run, sel, "/tmp/ws", "tok", mirror.Options{}, &bytes.Buffer{}, &bytes.Buffer{})
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

func TestMirrorCmdBranchWithShallowRejectedBeforeToken(t *testing.T) {
	// The --branch/--clone-depth guard is pure local flag logic and runs before
	// the token check: with no token set, the failure is still the clone-depth
	// error, not the missing-token one, proving the ordering.
	_, err := executeCmd(t, "", "mirror", "--branch", "dev", "--clone-depth", "1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--clone-depth")
	assert.NotContains(t, err.Error(), "GOLD_FINGER_PAT")
}
