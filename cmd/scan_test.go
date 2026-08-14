package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/redscaresu/goldfinger/models"
	"github.com/redscaresu/goldfinger/selection"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mkClone creates a git-clone-shaped repo dir at <ws>/<owner>/<name> (a directory
// holding a .git entry, which isGitClone requires) and writes the given files.
func mkClone(t *testing.T, ws, owner, name string, files map[string]string) {
	t.Helper()
	repoDir := filepath.Join(ws, owner, name)
	require.NoError(t, os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755))
	for rel, content := range files {
		writeFile(t, repoDir, rel, content)
	}
}

// scanSelection builds a selection over the named repos under owner.
func scanSelection(owner string, names ...string) models.Selection {
	repos := make([]models.Repo, len(names))
	for i, n := range names {
		repos[i] = models.Repo{Owner: owner, Name: n, DefaultBranch: "main"}
	}
	return models.Selection{
		Version:   models.SelectionVersion,
		Owner:     owner,
		OwnerType: models.OwnerOrganization,
		Repos:     repos,
	}
}

// runScanJSON runs runScan in JSON mode and decodes the stdout report.
func runScanJSON(t *testing.T, sel models.Selection, ws string, o scanOptions) (scanReport, string) {
	t.Helper()
	o.asJSON = true
	var out, errOut bytes.Buffer
	require.NoError(t, runScan(sel, ws, o, &out, &errOut))
	var rep scanReport
	require.NoError(t, json.Unmarshal(out.Bytes(), &rep), "stdout must be a single JSON object")
	return rep, errOut.String()
}

func TestRunScanReportsMatchesAndCounts(t *testing.T) {
	ws := t.TempDir()
	mkClone(t, ws, "acme", "a", map[string]string{"Dockerfile": "FROM debian:bullseye\n"})
	mkClone(t, ws, "acme", "b", map[string]string{"go.mod": "module b\n"})
	sel := scanSelection("acme", "a", "b")

	rep, _ := runScanJSON(t, sel, ws, scanOptions{pattern: "debian:bullseye"})

	assert.Equal(t, scanReportVersion, rep.Version)
	assert.Equal(t, "debian:bullseye", rep.Pattern)
	assert.Equal(t, ws, rep.Workspace)
	assert.Equal(t, "acme", rep.Owner)
	assert.Equal(t, 2, rep.ReposInSelection)
	assert.Equal(t, 2, rep.ReposScanned)
	assert.Equal(t, 1, rep.ReposWithMatches)
	assert.Equal(t, 0, rep.ReposNotScanned)
	assert.Equal(t, 1, rep.TotalMatches)
	assert.False(t, rep.Truncated)
	require.Len(t, rep.Repos, 2)

	// Repos are ordered by full name; each scanned repo carries a (possibly empty)
	// non-nil matches list.
	assert.Equal(t, "acme/a", rep.Repos[0].Repo)
	assert.True(t, rep.Repos[0].Scanned)
	require.Len(t, rep.Repos[0].Matches, 1)
	assert.Equal(t, "Dockerfile", rep.Repos[0].Matches[0].Path)
	assert.Equal(t, "acme/b", rep.Repos[1].Repo)
	assert.NotNil(t, rep.Repos[1].Matches)
	assert.Empty(t, rep.Repos[1].Matches)
}

func TestRunScanReportsNotMirroredReposNeverSilentlyDropped(t *testing.T) {
	ws := t.TempDir()
	mkClone(t, ws, "acme", "a", map[string]string{"x.txt": "hit\n"})
	// "b" is in the selection but never mirrored.
	sel := scanSelection("acme", "a", "b")

	rep, errOut := runScanJSON(t, sel, ws, scanOptions{pattern: "hit"})

	assert.Equal(t, 1, rep.ReposScanned)
	assert.Equal(t, 1, rep.ReposNotScanned)
	require.Len(t, rep.Repos, 2)
	b := rep.Repos[1]
	assert.Equal(t, "acme/b", b.Repo)
	assert.False(t, b.Scanned)
	assert.Equal(t, skipReasonNotMirrored, b.SkipReason)
	assert.Contains(t, errOut, "not on disk", "the human summary flags the coverage gap")
}

// TestRunScanRefusesSymlinkedOwnerEscapingWorkspace proves the owner directory is
// confined: a symlink at <workspace>/<owner> pointing at a real clone tree OUTSIDE
// the workspace must never be followed, or scan would read (and report matches from)
// a tree the workspace never contained — breaking provable-same-set. The os.Root
// confinement refuses it, so every selected repo reports not-scanned.
func TestRunScanRefusesSymlinkedOwnerEscapingWorkspace(t *testing.T) {
	ws := t.TempDir()
	realOwner := t.TempDir() // a clone tree OUTSIDE the workspace
	require.NoError(t, os.MkdirAll(filepath.Join(realOwner, "a", ".git"), 0o755))
	writeFile(t, filepath.Join(realOwner, "a"), "x.txt", "hit\n")
	require.NoError(t, os.Symlink(realOwner, filepath.Join(ws, "acme")))
	sel := scanSelection("acme", "a")

	rep, errOut := runScanJSON(t, sel, ws, scanOptions{pattern: "hit"})
	assert.Equal(t, 0, rep.ReposScanned, "a symlinked owner escaping the workspace must never be searched")
	assert.Equal(t, 1, rep.ReposNotScanned)
	require.Len(t, rep.Repos, 1)
	assert.False(t, rep.Repos[0].Scanned)
	assert.Equal(t, skipReasonNotMirrored, rep.Repos[0].SkipReason)
	assert.Contains(t, errOut, "not on disk")
}

// TestRunScanRefusesSymlinkedRepoEscapingWorkspace proves a repo entry that is a
// symlink pointing OUTSIDE the workspace is never followed: the Lstat gate sees a
// non-directory and reports it not-mirrored before any open.
func TestRunScanRefusesSymlinkedRepoEscapingWorkspace(t *testing.T) {
	ws := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "acme"), 0o755))
	outsideClone := filepath.Join(t.TempDir(), "realclone")
	require.NoError(t, os.MkdirAll(filepath.Join(outsideClone, ".git"), 0o755))
	writeFile(t, outsideClone, "x.txt", "hit\n")
	require.NoError(t, os.Symlink(outsideClone, filepath.Join(ws, "acme", "a")))
	sel := scanSelection("acme", "a")

	rep, _ := runScanJSON(t, sel, ws, scanOptions{pattern: "hit"})
	assert.Equal(t, 0, rep.ReposScanned, "a repo symlink escaping the workspace must never be searched")
	assert.Equal(t, 1, rep.ReposNotScanned)
	require.Len(t, rep.Repos, 1)
	assert.Equal(t, skipReasonNotMirrored, rep.Repos[0].SkipReason)
}

// TestRunScanRefusesInWorkspaceSymlinkedRepo proves an IN-workspace symlinked repo
// dir is refused too. os.Root would follow it (its target stays inside the root), so
// this guards the case the os.Root confinement alone does NOT block: scanning the
// target's tree under a different repo's name would break provable-same-set.
func TestRunScanRefusesInWorkspaceSymlinkedRepo(t *testing.T) {
	ws := t.TempDir()
	mkClone(t, ws, "acme", "target", map[string]string{"x.txt": "hit\n"})
	require.NoError(t, os.Symlink("target", filepath.Join(ws, "acme", "a")))
	sel := scanSelection("acme", "a") // 'a' is a symlink to 'target', not a real clone

	rep, _ := runScanJSON(t, sel, ws, scanOptions{pattern: "hit"})
	require.Len(t, rep.Repos, 1)
	assert.Equal(t, 0, rep.ReposScanned)
	assert.Equal(t, skipReasonNotMirrored, rep.Repos[0].SkipReason, "an in-workspace symlinked repo dir must not be followed")
}

// TestRunScanReportsUnreadableCloneDistinctFromNotMirrored locks Finding 3's
// classification: a clone that EXISTS but cannot be read (permissions, or a
// mid-scan swap) is reported unreadable — a re-runnable coverage gap — not
// conflated with a repo that was never mirrored.
func TestRunScanReportsUnreadableCloneDistinctFromNotMirrored(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission bits, so an unreadable clone cannot be simulated")
	}
	ws := t.TempDir()
	mkClone(t, ws, "acme", "a", map[string]string{"x.txt": "hit\n"})
	repo := filepath.Join(ws, "acme", "a")
	require.NoError(t, os.Chmod(repo, 0o000))
	t.Cleanup(func() { _ = os.Chmod(repo, 0o755) }) // let TempDir cleanup remove it
	sel := scanSelection("acme", "a")

	rep, errOut := runScanJSON(t, sel, ws, scanOptions{pattern: "hit"})
	assert.Equal(t, 0, rep.ReposScanned)
	assert.Equal(t, 1, rep.ReposNotScanned)
	require.Len(t, rep.Repos, 1)
	assert.Equal(t, skipReasonUnreadable, rep.Repos[0].SkipReason, "an existing-but-unreadable clone is unreadable, not not-mirrored")
	assert.Contains(t, errOut, "could not be read")
}

// TestRunScanIsProvableSameSet proves scan searches exactly the selection and no
// more: a repo present on disk but absent from the lockfile is never searched.
func TestRunScanIsProvableSameSet(t *testing.T) {
	ws := t.TempDir()
	mkClone(t, ws, "acme", "a", map[string]string{"x.txt": "hit\n"})
	mkClone(t, ws, "acme", "rogue", map[string]string{"x.txt": "hit\n"})
	sel := scanSelection("acme", "a") // rogue is on disk but not selected

	rep, _ := runScanJSON(t, sel, ws, scanOptions{pattern: "hit"})

	require.Len(t, rep.Repos, 1)
	assert.Equal(t, "acme/a", rep.Repos[0].Repo)
	assert.Equal(t, 1, rep.ReposScanned)
}

func TestRunScanReadsBranchFromSnapshotManifest(t *testing.T) {
	ws := t.TempDir()
	mkClone(t, ws, "acme", "a", map[string]string{"x.txt": "hit\n"})
	require.NoError(t, writeWorkspaceManifest(ws, workspaceManifest{
		Version: workspaceManifestVersion, Purpose: "audit", Branch: "dev",
		Stamp: "2026-08-14-000000.000", Owner: "acme",
	}))
	sel := scanSelection("acme", "a")

	rep, _ := runScanJSON(t, sel, ws, scanOptions{pattern: "hit"})
	assert.Equal(t, "dev", rep.Branch, "branch is read from the snapshot manifest")
}

func TestRunScanNoManifestOmitsBranch(t *testing.T) {
	ws := t.TempDir()
	mkClone(t, ws, "acme", "a", map[string]string{"x.txt": "hit\n"})
	sel := scanSelection("acme", "a")

	rep, _ := runScanJSON(t, sel, ws, scanOptions{pattern: "hit"})
	assert.Empty(t, rep.Branch, "a manifest-less workspace reports no branch")
}

func TestRunScanEmptySelectionErrors(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runScan(scanSelection("acme"), t.TempDir(), scanOptions{pattern: "x", asJSON: true}, &out, &errOut)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
	assert.Empty(t, out.String(), "no report is emitted for an empty selection")
}

func TestRunScanInvalidPatternErrors(t *testing.T) {
	ws := t.TempDir()
	mkClone(t, ws, "acme", "a", map[string]string{"x.txt": "y\n"})
	var out, errOut bytes.Buffer
	err := runScan(scanSelection("acme", "a"), ws, scanOptions{pattern: "(unclosed"}, &out, &errOut)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid pattern")
}

func TestRunScanFixedStringsMatchesLiterally(t *testing.T) {
	ws := t.TempDir()
	mkClone(t, ws, "acme", "a", map[string]string{"c.txt": "a.b\naxb\n"})
	sel := scanSelection("acme", "a")

	rep, _ := runScanJSON(t, sel, ws, scanOptions{pattern: "a.b", fixedStrings: true})
	require.Len(t, rep.Repos, 1)
	require.Len(t, rep.Repos[0].Matches, 1, "-F matches the literal a.b, not axb")
	assert.Equal(t, "a.b", rep.Repos[0].Matches[0].Text)
	assert.True(t, rep.FixedStrings)
}

// TestRunScanQuietStdoutIsPureJSON guards the stdout=data / stderr=noise contract:
// under --quiet the human summary is suppressed and stdout is exactly the report.
func TestRunScanQuietStdoutIsPureJSON(t *testing.T) {
	ws := t.TempDir()
	mkClone(t, ws, "acme", "a", map[string]string{"x.txt": "hit\n"})
	var out, errOut bytes.Buffer
	require.NoError(t, runScan(scanSelection("acme", "a"), ws, scanOptions{pattern: "hit", asJSON: true, quiet: true}, &out, &errOut))
	assert.Empty(t, errOut.String(), "quiet suppresses the human summary")
	var rep scanReport
	require.NoError(t, json.Unmarshal(out.Bytes(), &rep))
}

// TestScanCommandEndToEnd exercises the full cobra wiring (no token needed).
func TestScanCommandEndToEnd(t *testing.T) {
	ws := t.TempDir()
	mkClone(t, ws, "acme", "a", map[string]string{"Dockerfile": "FROM debian:bullseye\n"})
	selPath := filepath.Join(t.TempDir(), "goldfinger.selection")
	require.NoError(t, selection.Write(selPath, scanSelection("acme", "a"), selection.WriteOptions{Overwrite: true}))

	// executeCmd merges stdout+stderr into one buffer; --quiet suppresses the
	// human summary so the buffer is exactly the JSON report.
	out, err := executeCmd(t, "", "scan", "--selection", selPath, "--workspace", ws, "--json", "--quiet", "debian:bullseye")
	require.NoError(t, err)
	var rep scanReport
	require.NoError(t, json.Unmarshal([]byte(out), &rep))
	assert.Equal(t, 1, rep.TotalMatches)
}
