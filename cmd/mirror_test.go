package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
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
	ws, snap, err := resolveWorkspace("", "", "")
	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(ws))
	assert.True(t, strings.HasSuffix(ws, "goldfinger"))
	assert.Nil(t, snap, "the default workspace is persistent, not a managed snapshot")
}

func TestResolveWorkspaceRelativeBecomesAbsolute(t *testing.T) {
	ws, snap, err := resolveWorkspace("some/dir", "", "")
	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(ws))
	assert.True(t, strings.HasSuffix(ws, filepath.Join("some", "dir")))
	assert.Nil(t, snap, "an explicit --workspace is not a managed snapshot")
}

func TestResolveWorkspacePurposeIsTimestamped(t *testing.T) {
	orig := nowFunc
	now := time.Date(2026, 8, 4, 13, 20, 45, 123000000, time.UTC)
	nowFunc = func() time.Time { return now }
	defer func() { nowFunc = orig }()

	ws, snap, err := resolveWorkspace("", "keyv-cve", "")
	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(ws))
	// goldfinger stamps the time to the millisecond; the operator supplied only
	// the purpose, so each run gets its own pristine dir.
	assert.True(t, strings.HasSuffix(ws, filepath.Join("goldfinger", "keyv-cve-2026-08-04-132045.123")),
		"want ~/goldfinger/keyv-cve-2026-08-04-132045.123, got %s", ws)
	// A --purpose snapshot carries a manifest matching the stamped dir name (Owner
	// is filled by the caller from the selection, so it is empty here).
	require.NotNil(t, snap)
	assert.Equal(t, workspaceManifest{
		Version: workspaceManifestVersion, Purpose: "keyv-cve",
		Stamp: "2026-08-04-132045.123", CreatedAt: now,
	}, *snap)
}

func TestResolveWorkspacePurposeFoldsInBranch(t *testing.T) {
	orig := nowFunc
	nowFunc = func() time.Time { return time.Date(2026, 8, 4, 13, 20, 45, 123000000, time.UTC) }
	defer func() { nowFunc = orig }()

	// A branch with a slash must fold into a single safe path segment.
	ws, snap, err := resolveWorkspace("", "keyv-cve", "feature/x")
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(ws, filepath.Join("goldfinger", "keyv-cve-feature-x-2026-08-04-132045.123")),
		"want ~/goldfinger/keyv-cve-feature-x-2026-08-04-132045.123, got %s", ws)
	// The manifest records the REAL branch (slashes intact), not the sanitised
	// dir-name component — that is the reliability win of the sidecar.
	require.NotNil(t, snap)
	assert.Equal(t, "feature/x", snap.Branch)
}

func TestResolveWorkspacePurposeAndWorkspaceMutuallyExclusive(t *testing.T) {
	_, _, err := resolveWorkspace("/some/ws", "keyv-cve", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

func TestResolveWorkspaceRejectsUnsafePurpose(t *testing.T) {
	for _, p := range []string{"../etc", "a/b", "has space", "..", "with\\slash"} {
		_, _, err := resolveWorkspace("", p, "")
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
		err := runMirror(context.Background(), run, sel, "/tmp/ws", "tok", mirror.Options{}, reportOptions{}, &out, &errOut)
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
		err := runMirror(context.Background(), run, sel, "/tmp/ws", "tok", mirror.Options{}, reportOptions{}, &bytes.Buffer{}, &bytes.Buffer{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ghorg blew up")
	})

	t.Run("empty selection fails with no misleading stdout", func(t *testing.T) {
		empty := models.Selection{Owner: "redscaresu", OwnerType: models.OwnerUser}
		called := false
		run := func(_ context.Context, _ string, _, _ []string) error { called = true; return nil }
		var out, errOut bytes.Buffer
		err := runMirror(context.Background(), run, empty, "/tmp/ws", "tok", mirror.Options{}, reportOptions{}, &out, &errOut)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "selection is empty")
		// The workspace path must NOT have been printed — an agent reading stdout
		// would otherwise think a mirror happened.
		assert.Empty(t, out.String(), "no machine stdout before failing on empty selection")
		assert.False(t, called, "delegate must not be invoked for an empty selection")
	})
}

func TestBuildMirrorReportCategorises(t *testing.T) {
	sel := models.Selection{
		Owner:           "acme",
		OwnerType:       models.OwnerUser,
		BranchesChecked: []string{"dev"},
		Repos: []models.Repo{
			{Owner: "acme", Name: "has-it", DefaultBranch: "main", BranchPresence: map[string]bool{"dev": true}},
			{Owner: "acme", Name: "lacks-it", DefaultBranch: "main", BranchPresence: map[string]bool{"dev": false}},
			{Owner: "acme", Name: "default-is-dev", DefaultBranch: "dev"},
			{Owner: "acme", Name: "never-checked", DefaultBranch: "main"}, // no BranchPresence -> unknown
		},
	}
	rep := buildMirrorReport(sel, "/tmp/ws", mirror.Options{Branch: "dev"})

	assert.Equal(t, "/tmp/ws", rep.Workspace)
	assert.Equal(t, "acme", rep.Owner)
	assert.Equal(t, 4, rep.RepoCount)
	assert.Equal(t, "dev", rep.Branch)
	assert.NotEmpty(t, rep.BranchFactsNote, "a requested branch carries the drift caveat")

	status := map[string]string{}
	for _, r := range rep.Repos {
		status[r.Repo] = r.BranchStatus
	}
	assert.Equal(t, branchStatusHas, status["acme/has-it"])
	assert.Equal(t, branchStatusFallback, status["acme/lacks-it"])
	assert.Equal(t, branchStatusHas, status["acme/default-is-dev"], "default branch is present by definition")
	assert.Equal(t, branchStatusUnknown, status["acme/never-checked"], "unchecked branch is unknown, not guessed")
}

func TestBuildMirrorReportNoBranch(t *testing.T) {
	sel := models.Selection{Owner: "acme", Repos: []models.Repo{{Owner: "acme", Name: "a", DefaultBranch: "main"}}}
	rep := buildMirrorReport(sel, "/tmp/ws", mirror.Options{})

	assert.Empty(t, rep.Branch)
	assert.Empty(t, rep.BranchFactsNote, "no requested branch, no branch caveat")
	require.Len(t, rep.Repos, 1)
	assert.Equal(t, branchStatusDefault, rep.Repos[0].BranchStatus)
}

func TestRunMirrorReport(t *testing.T) {
	sel := models.Selection{
		Owner: "acme", OwnerType: models.OwnerUser,
		BranchesChecked: []string{"dev"},
		Repos:           []models.Repo{{Owner: "acme", Name: "svc", DefaultBranch: "main", BranchPresence: map[string]bool{"dev": true}}},
	}

	t.Run("writes report to stdout and file on success", func(t *testing.T) {
		ws := t.TempDir()
		run := func(_ context.Context, _ string, _, _ []string) error { return nil }
		var out bytes.Buffer
		err := runMirror(context.Background(), run, sel, ws, "tok", mirror.Options{Branch: "dev"},
			reportOptions{toStdout: true, toFile: true}, &out, &bytes.Buffer{})
		require.NoError(t, err)

		var fromStdout mirrorReport
		require.NoError(t, json.Unmarshal(out.Bytes(), &fromStdout))
		assert.Equal(t, ws, fromStdout.Workspace)

		data, err := os.ReadFile(filepath.Join(ws, mirrorReportName))
		require.NoError(t, err)
		var fromFile mirrorReport
		require.NoError(t, json.Unmarshal(data, &fromFile))
		require.Len(t, fromFile.Repos, 1)
		assert.Equal(t, branchStatusHas, fromFile.Repos[0].BranchStatus)
	})

	// Regression: --report-json and the bare workspace-path line both target
	// stdout, so integrating #15-D (path line) with #15-C (JSON report) risked
	// emitting BOTH — a "/tmp/ws\n{...}" stream that no JSON reader can parse.
	// stdout must carry the JSON alone in report mode.
	t.Run("report mode suppresses the bare workspace-path line on stdout", func(t *testing.T) {
		ws := t.TempDir()
		run := func(_ context.Context, _ string, _, _ []string) error { return nil }
		var out bytes.Buffer
		err := runMirror(context.Background(), run, sel, ws, "tok", mirror.Options{Branch: "dev"},
			reportOptions{toStdout: true}, &out, &bytes.Buffer{})
		require.NoError(t, err)
		assert.False(t, strings.HasPrefix(out.String(), ws+"\n"),
			"report mode must not prepend the bare path line before the JSON")
		// The whole of stdout is one JSON document — nothing else leaked onto it.
		var rep mirrorReport
		require.NoError(t, json.Unmarshal(out.Bytes(), &rep))
		assert.Equal(t, ws, rep.Workspace)
	})

	t.Run("no report file when the mirror fails", func(t *testing.T) {
		ws := t.TempDir()
		run := func(_ context.Context, _ string, _, _ []string) error { return errors.New("boom") }
		err := runMirror(context.Background(), run, sel, ws, "tok", mirror.Options{Branch: "dev"},
			reportOptions{toFile: true}, &bytes.Buffer{}, &bytes.Buffer{})
		require.Error(t, err)
		_, statErr := os.Stat(filepath.Join(ws, mirrorReportName))
		assert.True(t, os.IsNotExist(statErr), "a failed mirror must not leave a report claiming success")
	})

	t.Run("no report on a dry-run", func(t *testing.T) {
		ws := t.TempDir()
		run := func(_ context.Context, _ string, _, _ []string) error { return nil }
		var out bytes.Buffer
		err := runMirror(context.Background(), run, sel, ws, "tok", mirror.Options{Branch: "dev", DryRun: true},
			reportOptions{toStdout: true, toFile: true}, &out, &bytes.Buffer{})
		require.NoError(t, err)
		assert.Empty(t, out.String(), "a dry-run clones nothing, so it emits no stdout report")
		_, statErr := os.Stat(filepath.Join(ws, mirrorReportName))
		assert.True(t, os.IsNotExist(statErr), "a dry-run must not write a report file")
	})
}

func TestMirrorCmdMissingToken(t *testing.T) {
	// Local guards (flag combos, selection read, workspace) run before auth, so a
	// readable selection is needed to reach the token check. resolveToken precedes
	// the ghorg probe, so this stays deterministic regardless of whether ghorg is
	// installed.
	sel := writeSelection(t)
	_, err := executeCmd(t, "", "mirror", "--selection", sel)
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
