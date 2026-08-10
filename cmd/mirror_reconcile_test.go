package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/redscaresu/goldfinger/mirror"
	"github.com/redscaresu/goldfinger/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mkRepoDirs creates <ws>/<owner>/<name>/.git for each name, mimicking where
// ghorg lands clones (a real clone has a .git dir), so reconcile's read-only
// on-disk count sees genuine clones rather than bare directories.
func mkRepoDirs(t *testing.T, ws, owner string, names ...string) {
	t.Helper()
	for _, n := range names {
		require.NoError(t, os.MkdirAll(filepath.Join(ws, owner, n, ".git"), 0o755))
	}
}

func TestReconcileCountsOnlyReposThatLanded(t *testing.T) {
	ws := t.TempDir()
	sel := models.Selection{Owner: "acme", Repos: []models.Repo{
		{Owner: "acme", Name: "a"}, {Owner: "acme", Name: "b"}, {Owner: "acme", Name: "c"},
	}}
	// Only two of the three selected repos actually cloned.
	mkRepoDirs(t, ws, "acme", "a", "b")

	rec := reconcile(sel, ws, mirror.Options{})
	assert.Equal(t, 3, rec.inSelection)
	assert.Equal(t, 2, rec.onDisk)
	assert.Equal(t, 1, rec.shortfall(), "a selected repo missing on disk is a coverage shortfall")
	assert.False(t, rec.hasBranch)
}

func TestReconcileRequiresADirNotJustAPath(t *testing.T) {
	// A selected repo whose path exists as a FILE (not a clone directory) must not
	// count as "on disk" — locking the isDir check against a regression to a mere
	// existence test, which would falsely report coverage for a stray file.
	ws := t.TempDir()
	sel := models.Selection{Owner: "acme", Repos: []models.Repo{{Owner: "acme", Name: "a"}}}
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "acme"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "acme", "a"), []byte("x"), 0o644))

	rec := reconcile(sel, ws, mirror.Options{})
	assert.Equal(t, 0, rec.onDisk, "a file at the repo path is not a clone and must not count")
	assert.Equal(t, 1, rec.shortfall())
}

func TestReconcileRequiresAGitDirNotJustAnyDir(t *testing.T) {
	// A leftover or half-written directory from an earlier interrupted mirror —
	// one that exists but has no .git — must NOT count as covered. This is the
	// core of the "authoritative coverage" honesty fix: only a real clone counts.
	ws := t.TempDir()
	sel := models.Selection{Owner: "acme", Repos: []models.Repo{
		{Owner: "acme", Name: "landed"}, {Owner: "acme", Name: "stale"},
	}}
	mkRepoDirs(t, ws, "acme", "landed")                                          // real clone (.git present)
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "acme", "stale"), 0o755))   // bare dir, no .git

	rec := reconcile(sel, ws, mirror.Options{})
	assert.Equal(t, 1, rec.onDisk, "a directory without .git is not a clone and must not count")
	assert.Equal(t, 1, rec.shortfall(), "the stale dir is a real coverage shortfall")
}

func TestReconcileAcceptsGitfileWorktree(t *testing.T) {
	// A .git *file* (a gitfile-linked worktree) is still a real clone and must
	// count, so the check doesn't over-narrow to only ghorg's usual .git dir.
	ws := t.TempDir()
	sel := models.Selection{Owner: "acme", Repos: []models.Repo{{Owner: "acme", Name: "wt"}}}
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "acme", "wt"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "acme", "wt", ".git"), []byte("gitdir: /elsewhere\n"), 0o644))

	rec := reconcile(sel, ws, mirror.Options{})
	assert.Equal(t, 1, rec.onDisk, "a .git file (worktree) is still a clone")
	assert.Equal(t, 0, rec.shortfall())
}

func TestReconcileBranchTallies(t *testing.T) {
	ws := t.TempDir()
	sel := models.Selection{
		Owner: "acme", BranchesChecked: []string{"dev"},
		Repos: []models.Repo{
			{Owner: "acme", Name: "has", DefaultBranch: "main", BranchPresence: map[string]bool{"dev": true}},
			{Owner: "acme", Name: "lacks", DefaultBranch: "main", BranchPresence: map[string]bool{"dev": false}},
			{Owner: "acme", Name: "default-dev", DefaultBranch: "dev"},
			{Owner: "acme", Name: "unchecked", DefaultBranch: "main"},
		},
	}
	mkRepoDirs(t, ws, "acme", "has", "lacks", "default-dev", "unchecked")

	rec := reconcile(sel, ws, mirror.Options{Branch: "dev"})
	assert.True(t, rec.hasBranch)
	assert.Equal(t, 4, rec.onDisk)
	assert.Equal(t, 2, rec.branchPresent, "an explicit dev plus a repo whose default is dev")
	assert.Equal(t, 1, rec.fellBack)
	assert.Equal(t, 1, rec.unknown)
	// The branch tallies partition the selection exactly.
	assert.Equal(t, rec.inSelection, rec.branchPresent+rec.fellBack+rec.unknown)
}

func TestReconciliationLineNoBranch(t *testing.T) {
	rec := reconciliation{inSelection: 59, onDisk: 59}
	assert.Equal(t, "in selection: 59 | on disk: 59", rec.line())
}

func TestReconciliationLineWithBranch(t *testing.T) {
	rec := reconciliation{inSelection: 59, onDisk: 59, hasBranch: true, branchPresent: 15, fellBack: 44}
	assert.Equal(t, "in selection: 59 | on disk: 59 | branch present: 15 | fell back: 44", rec.line())
}

func TestReconciliationLineHidesZeroUnknownShowsNonZero(t *testing.T) {
	terse := reconciliation{inSelection: 10, onDisk: 10, hasBranch: true, branchPresent: 6, fellBack: 4}
	assert.NotContains(t, terse.line(), "unknown", "a zero unknown tally is omitted to keep the line terse")

	withUnknown := reconciliation{inSelection: 10, onDisk: 10, hasBranch: true, branchPresent: 6, fellBack: 2, unknown: 2}
	assert.Equal(t, "in selection: 10 | on disk: 10 | branch present: 6 | fell back: 2 | unknown: 2", withUnknown.line())
}

func TestReportReconciliationFullMirrorSucceeds(t *testing.T) {
	ws := t.TempDir()
	sel := models.Selection{Owner: "acme", Repos: []models.Repo{{Owner: "acme", Name: "a"}, {Owner: "acme", Name: "b"}}}
	mkRepoDirs(t, ws, "acme", "a", "b")

	var errOut bytes.Buffer
	rec := reconcile(sel, ws, mirror.Options{})
	reportReconciliation(&errOut, rec, ws, sel.Owner, "")
	s := errOut.String()
	assert.Contains(t, s, "reconciliation: in selection: 2 | on disk: 2")
	assert.NotContains(t, s, "under-covered", "a full mirror must not warn about a shortfall")
}

func TestReportReconciliationWarnsOnShortfall(t *testing.T) {
	ws := t.TempDir()
	sel := models.Selection{Owner: "acme", Repos: []models.Repo{{Owner: "acme", Name: "a"}, {Owner: "acme", Name: "gone"}}}
	mkRepoDirs(t, ws, "acme", "a") // "gone" never landed

	var errOut bytes.Buffer
	rec := reconcile(sel, ws, mirror.Options{})
	reportReconciliation(&errOut, rec, ws, sel.Owner, "")
	s := errOut.String()
	assert.Contains(t, s, "in selection: 2 | on disk: 1")
	assert.Contains(t, s, "1 selected repo(s) are not on disk")
	assert.Contains(t, s, "under-covered")
}

func TestReportReconciliationShortfallPointsAtGhorgLog(t *testing.T) {
	// When a ghorg log was captured, the shortfall hint names it so an operator
	// can drill into the clone errors behind the terse summary.
	ws := t.TempDir()
	sel := models.Selection{Owner: "acme", Repos: []models.Repo{{Owner: "acme", Name: "a"}, {Owner: "acme", Name: "gone"}}}
	mkRepoDirs(t, ws, "acme", "a")

	var errOut bytes.Buffer
	rec := reconcile(sel, ws, mirror.Options{})
	reportReconciliation(&errOut, rec, ws, sel.Owner, "/tmp/goldfinger-mirror-output-xyz.log")
	s := errOut.String()
	assert.Contains(t, s, "check the captured ghorg log for clone errors: /tmp/goldfinger-mirror-output-xyz.log")
}
