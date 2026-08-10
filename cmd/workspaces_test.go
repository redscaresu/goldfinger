package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runWS runs the workspaces core with separated streams and returns stdout — the
// machine-data channel — so a test can parse the JSON report without the stderr
// banners (which carry non-ASCII em-dashes) bleeding into it. executeCmd merges
// the two, so it is only used where a test asserts on errors or filesystem state.
func runWS(t *testing.T, opts workspacesOptions) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := runWorkspaces(opts, &out, &errOut)
	return out.String(), err
}

// makeSnapshot creates a snapshot directory <root>/<name> containing one file of
// the given size, and — when purpose != "" — a sidecar manifest. It returns the
// snapshot path.
func makeSnapshot(t *testing.T, root, name, purpose, branch string, created time.Time, sizeBytes int) string {
	t.Helper()
	dir := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "payload.bin"), make([]byte, sizeBytes), 0o644))
	if purpose != "" {
		m := workspaceManifest{
			Version: workspaceManifestVersion, Purpose: purpose, Branch: branch,
			Stamp: strings.TrimPrefix(name[strings.LastIndex(name, "-20"):], "-"), Owner: "acme",
			CreatedAt: created,
		}
		data, err := json.MarshalIndent(m, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, workspaceManifestName), data, 0o644))
	}
	return dir
}

func TestSnapshotStampRecognisesOnlyStampedDirs(t *testing.T) {
	cases := map[string]bool{
		"audit-2026-08-05-101112.131":              true,
		"audit-dev-2026-08-05-101112.131":          true,
		"keyv-cve-feature-x-2026-01-02-030405.006": true,
		"acme":                           false, // a plain owner dir (default mirror)
		"goldfinger-mirror.json":         false,
		"audit-2026-08-05":               false, // truncated stamp
		"2026-08-05-101112.131-trailing": false, // stamp not at the end
	}
	for name, want := range cases {
		_, ok := snapshotStamp(name)
		assert.Equalf(t, want, ok, "snapshotStamp(%q)", name)
	}
}

func TestListEnumeratesSnapshotsAndIgnoresOtherDirs(t *testing.T) {
	root := t.TempDir()
	created := time.Date(2026, 8, 5, 10, 11, 12, 131000000, time.UTC)
	makeSnapshot(t, root, "audit-2026-08-05-101112.131", "audit", "", created, 1024)
	// A default-mirror owner dir and a stray file must be ignored.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "acme", "repo"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "loose.txt"), []byte("x"), 0o644))

	ws, err := scanWorkspaces(root)
	require.NoError(t, err)
	require.Len(t, ws, 1, "only the stamped snapshot dir is a workspace")
	assert.Equal(t, "audit", ws[0].Purpose)
	assert.True(t, ws[0].ManifestPresent)
	assert.Equal(t, "acme", ws[0].Owner)
	assert.Greater(t, ws[0].SizeBytes, int64(1024), "size includes the manifest + payload")
}

func TestScanMissingRootIsEmptyNotError(t *testing.T) {
	ws, err := scanWorkspaces(filepath.Join(t.TempDir(), "never-mirrored"))
	require.NoError(t, err)
	assert.Empty(t, ws)
}

func TestManifestlessSnapshotStillListedWithParsedTime(t *testing.T) {
	root := t.TempDir()
	makeSnapshot(t, root, "legacy-2026-01-02-030405.006", "", "", time.Time{}, 10)
	ws, err := scanWorkspaces(root)
	require.NoError(t, err)
	require.Len(t, ws, 1)
	assert.False(t, ws[0].ManifestPresent)
	assert.Empty(t, ws[0].Purpose, "purpose is unknown without a manifest")
	assert.Equal(t, "2026-01-02-030405.006", ws[0].Stamp)
	// createdAt is recovered from the dir-name stamp even without a manifest.
	assert.NotEmpty(t, ws[0].CreatedAt)
}

func TestListJSONShape(t *testing.T) {
	root := t.TempDir()
	created := time.Date(2026, 8, 5, 10, 11, 12, 131000000, time.UTC)
	makeSnapshot(t, root, "audit-2026-08-05-101112.131", "audit", "", created, 512)

	out, err := runWS(t, workspacesOptions{action: workspaceActionList, root: root, asJSON: true})
	require.NoError(t, err)

	var rep workspacesReport
	require.NoError(t, json.Unmarshal([]byte(out), &rep))
	assert.Equal(t, workspacesReportVersion, rep.Version)
	assert.Equal(t, workspaceActionList, rep.Action)
	assert.False(t, rep.Pruned)
	require.Len(t, rep.Workspaces, 1)
	assert.Equal(t, "audit", rep.Workspaces[0].Purpose)
}

func TestWorkspacesQuiet(t *testing.T) {
	root := t.TempDir()
	created := time.Date(2026, 8, 5, 10, 11, 12, 131000000, time.UTC)
	dir := makeSnapshot(t, root, "audit-2026-08-05-101112.131", "audit", "", created, 512)

	t.Run("list non-json emits nothing", func(t *testing.T) {
		var out, errOut bytes.Buffer
		err := runWorkspaces(workspacesOptions{action: workspaceActionList, root: root, quiet: true}, &out, &errOut)
		require.NoError(t, err)
		assert.Empty(t, out.String())
		assert.Empty(t, errOut.String())
	})

	t.Run("list json emits report only", func(t *testing.T) {
		var out, errOut bytes.Buffer
		err := runWorkspaces(workspacesOptions{action: workspaceActionList, root: root, asJSON: true, quiet: true}, &out, &errOut)
		require.NoError(t, err)
		var rep workspacesReport
		require.NoError(t, json.Unmarshal(out.Bytes(), &rep))
		require.Len(t, rep.Workspaces, 1)
		assert.Empty(t, errOut.String())
	})

	t.Run("prune preview emits nothing and deletes nothing", func(t *testing.T) {
		var out, errOut bytes.Buffer
		err := runWorkspaces(workspacesOptions{action: workspaceActionPrune, root: root, quiet: true}, &out, &errOut)
		require.NoError(t, err)
		assert.Empty(t, out.String())
		assert.Empty(t, errOut.String())
		assert.DirExists(t, dir)
	})

	t.Run("prune confirm deletes quietly", func(t *testing.T) {
		var out, errOut bytes.Buffer
		err := runWorkspaces(workspacesOptions{action: workspaceActionPrune, root: root, confirm: true, quiet: true}, &out, &errOut)
		require.NoError(t, err)
		assert.Empty(t, out.String())
		assert.Empty(t, errOut.String())
		assert.NoDirExists(t, dir)
	})
}

func TestListRejectsPruneOnlyFlags(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][]string{
		{"workspaces", "list", "--root", root, "--confirm"},
		{"workspaces", "list", "--root", root, "--older-than", "1h"},
		{"workspaces", "list", "--root", root, "--purpose", "audit"},
	} {
		_, err := executeCmd(t, "tok", args...)
		require.Errorf(t, err, "args %v should be rejected", args)
		assert.Contains(t, err.Error(), "prune")
	}
}

func TestUnknownActionIsRejected(t *testing.T) {
	_, err := executeCmd(t, "tok", "workspaces", "wipe", "--root", t.TempDir())
	require.Error(t, err)
}

func TestPrunePreviewDeletesNothing(t *testing.T) {
	root := t.TempDir()
	created := time.Date(2026, 8, 5, 10, 11, 12, 131000000, time.UTC)
	dir := makeSnapshot(t, root, "audit-2026-08-05-101112.131", "audit", "", created, 128)

	out, err := runWS(t, workspacesOptions{action: workspaceActionPrune, root: root, asJSON: true})
	require.NoError(t, err)

	var rep workspacesReport
	require.NoError(t, json.Unmarshal([]byte(out), &rep))
	assert.Equal(t, workspaceActionPrune, rep.Action)
	assert.False(t, rep.Pruned, "preview must not report a deletion")
	require.Len(t, rep.Workspaces, 1)
	assert.DirExists(t, dir, "preview must not delete anything")
}

func TestPruneConfirmDeletesMatches(t *testing.T) {
	root := t.TempDir()
	created := time.Date(2026, 8, 5, 10, 11, 12, 131000000, time.UTC)
	dir := makeSnapshot(t, root, "audit-2026-08-05-101112.131", "audit", "", created, 128)

	out, err := runWS(t, workspacesOptions{action: workspaceActionPrune, root: root, confirm: true, asJSON: true})
	require.NoError(t, err)

	var rep workspacesReport
	require.NoError(t, json.Unmarshal([]byte(out), &rep))
	assert.True(t, rep.Pruned)
	assert.NoDirExists(t, dir, "--confirm must delete the matched snapshot")
}

func TestPrunePurposeMatchesManifestOnly(t *testing.T) {
	root := t.TempDir()
	created := time.Date(2026, 8, 5, 10, 11, 12, 131000000, time.UTC)
	keep := makeSnapshot(t, root, "audit-2026-08-05-101112.131", "audit", "", created, 100)
	other := makeSnapshot(t, root, "bump-2026-08-05-101112.132", "bump", "", created, 100)
	legacy := makeSnapshot(t, root, "audit-2026-08-05-101112.133", "", "", created, 100) // same name-ish, no manifest

	_, err := executeCmd(t, "tok", "workspaces", "prune", "--root", root, "--purpose", "bump", "--confirm")
	require.NoError(t, err)

	assert.NoDirExists(t, other, "the manifest purpose=bump snapshot is removed")
	assert.DirExists(t, keep, "a different purpose is untouched")
	assert.DirExists(t, legacy, "a manifest-less snapshot is never matched by --purpose")
}

func TestPruneOlderThanFiltersByAge(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	orig := nowFunc
	nowFunc = func() time.Time { return now }
	defer func() { nowFunc = orig }()

	old := makeSnapshot(t, root, "audit-2026-08-01-000000.000", "audit", "", now.Add(-9*24*time.Hour), 100)
	fresh := makeSnapshot(t, root, "audit-2026-08-09-000000.000", "audit", "", now.Add(-24*time.Hour), 100)

	_, err := executeCmd(t, "tok", "workspaces", "prune", "--root", root, "--older-than", "168h", "--confirm")
	require.NoError(t, err)

	assert.NoDirExists(t, old, "a snapshot older than 7d is pruned")
	assert.DirExists(t, fresh, "a snapshot within 7d is kept")
}

func TestPruneOlderThanAcceptsDayWeekSugar(t *testing.T) {
	// "7d" must behave exactly like "168h": the 9-day-old snapshot goes, the
	// 1-day-old one stays. This locks the day/week sugar to the same filter path
	// as a Go duration.
	root := t.TempDir()
	now := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	orig := nowFunc
	nowFunc = func() time.Time { return now }
	defer func() { nowFunc = orig }()

	old := makeSnapshot(t, root, "audit-2026-08-01-000000.000", "audit", "", now.Add(-9*24*time.Hour), 100)
	fresh := makeSnapshot(t, root, "audit-2026-08-09-000000.000", "audit", "", now.Add(-24*time.Hour), 100)

	_, err := executeCmd(t, "tok", "workspaces", "prune", "--root", root, "--older-than", "7d", "--confirm")
	require.NoError(t, err)

	assert.NoDirExists(t, old, "a snapshot older than 7d is pruned via the day-sugar form")
	assert.DirExists(t, fresh, "a snapshot within 7d is kept")
}

func TestParseAgeDuration(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{in: "7d", want: 168 * time.Hour},
		{in: "1d", want: 24 * time.Hour},
		{in: "2w", want: 336 * time.Hour},
		{in: "168h", want: 168 * time.Hour},
		{in: "90m", want: 90 * time.Minute},
		{in: "1h30m", want: 90 * time.Minute},
		{in: "0", want: 0},
		{in: "-7d", want: -168 * time.Hour}, // parses; runWorkspaces rejects the sign
		{in: "", wantErr: true},
		{in: "7dd", wantErr: true},
		{in: "1w3d", wantErr: true}, // no compounding of the sugar units
		{in: "7days", wantErr: true},
		{in: "d", wantErr: true},
		{in: "abc", wantErr: true},
		// Overflow guard: n*unit must not wrap int64 nanoseconds. 106752 days and
		// 15251 weeks are each one step past the largest value that fits, and a huge
		// negative would wrap toward 0 and slip past the negative-age reject.
		{in: "106752d", wantErr: true},
		{in: "15251w", wantErr: true},
		{in: "-106752d", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := parseAgeDuration(c.in)
			if c.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
		})
	}
}

func TestPruneRejectsNegativeDaySugar(t *testing.T) {
	// "-7d" must reach the same negative-age rejection as "-1h", not fall through
	// to time.ParseDuration's opaque "unknown unit d" error.
	root := t.TempDir()
	created := time.Date(2026, 8, 5, 10, 11, 12, 131000000, time.UTC)
	dir := makeSnapshot(t, root, "audit-2026-08-05-101112.131", "audit", "", created, 100)

	_, err := executeCmd(t, "tok", "workspaces", "prune", "--root", root, "--older-than", "-7d", "--confirm")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "negative")
	assert.DirExists(t, dir, "a rejected filter must not delete anything")
}

func TestPruneRejectsOverflowingDaySugar(t *testing.T) {
	// Regression: a day/week count large enough to overflow int64 nanoseconds used
	// to wrap silently — 213504d wrapped to ~25m, slipping past the non-negative
	// guard so `prune --confirm` would match (and delete) almost every snapshot.
	// The out-of-range age must be rejected at flag-parse time, deleting nothing.
	root := t.TempDir()
	created := time.Date(2026, 8, 5, 10, 11, 12, 131000000, time.UTC)
	dir := makeSnapshot(t, root, "audit-2026-08-05-101112.131", "audit", "", created, 100)

	_, err := executeCmd(t, "tok", "workspaces", "prune", "--root", root, "--older-than", "213504d", "--confirm")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "out of range")
	assert.DirExists(t, dir, "an overflowing age must be rejected, never wrap into a delete-everything filter")
}

func TestPruneRejectsNegativeOlderThan(t *testing.T) {
	root := t.TempDir()
	created := time.Date(2026, 8, 5, 10, 11, 12, 131000000, time.UTC)
	dir := makeSnapshot(t, root, "audit-2026-08-05-101112.131", "audit", "", created, 100)

	// A negative age must be rejected, never silently treated as "no filter"
	// (which would match — and with --confirm, delete — every snapshot).
	_, err := executeCmd(t, "tok", "workspaces", "prune", "--root", root, "--older-than", "-1h", "--confirm")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "negative")
	assert.DirExists(t, dir, "a rejected filter must not delete anything")
}

func TestPruneRefusesSymlinkedSnapshot(t *testing.T) {
	root := t.TempDir()
	// A stamp-named entry that is a symlink to an outside directory passes the
	// lexical parent-dir check but must still be refused: os.RemoveAll would
	// otherwise unlink it (and a careless change could follow it).
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "precious.txt"), []byte("keep"), 0o644))
	link := filepath.Join(root, "audit-2026-08-05-101112.131")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	err := deleteWorkspaces(root, []workspaceInfo{{Path: link}}, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a directory")
	assert.FileExists(t, filepath.Join(outside, "precious.txt"), "the symlink target is untouched")
}

func TestSafeToRemoveGuard(t *testing.T) {
	root := "/home/u/goldfinger"
	assert.True(t, safeToRemove(root, filepath.Join(root, "audit-2026-08-05-101112.131")))
	assert.False(t, safeToRemove(root, filepath.Join(root, "acme")), "a plain owner dir is not removable")
	assert.False(t, safeToRemove(root, filepath.Join(root, "sub", "audit-2026-08-05-101112.131")), "must be a direct child")
	assert.False(t, safeToRemove(root, root), "the root itself is never removable")
}

func TestFilterForPruneNoFilterMatchesAll(t *testing.T) {
	all := []workspaceInfo{
		{Path: "/r/a-2026-08-05-101112.131", ManifestPresent: true, Purpose: "a"},
		{Path: "/r/b-2026-08-05-101112.132", ManifestPresent: false},
	}
	got := filterForPrune(all, 0, "", time.Now())
	assert.Len(t, got, 2, "no filters => every snapshot matches (still confirm-gated)")
}

func TestHumanSize(t *testing.T) {
	assert.Equal(t, "512 B", humanSize(512))
	assert.Equal(t, "1.0 KiB", humanSize(1024))
	assert.Equal(t, "1.5 KiB", humanSize(1536))
	assert.Equal(t, "1.0 MiB", humanSize(1024*1024))
}

func TestWriteSnapshotManifestRoundTrips(t *testing.T) {
	ws := t.TempDir()
	snap := &workspaceManifest{
		Version: workspaceManifestVersion, Purpose: "audit", Branch: "dev",
		Stamp: "2026-08-05-101112.131", Owner: "acme",
		CreatedAt: time.Date(2026, 8, 5, 10, 11, 12, 131000000, time.UTC),
	}
	require.NoError(t, writeSnapshotManifest(ws, snap, false))

	got, ok := readWorkspaceManifest(ws)
	require.True(t, ok)
	assert.Equal(t, "audit", got.Purpose)
	assert.Equal(t, "dev", got.Branch)
	assert.Equal(t, "acme", got.Owner)
}

func TestWriteSnapshotManifestSkipsDryRunAndNilSnap(t *testing.T) {
	ws := t.TempDir()
	// dry-run: nothing written even for a real snapshot.
	require.NoError(t, writeSnapshotManifest(ws, &workspaceManifest{Purpose: "audit"}, true))
	_, ok := readWorkspaceManifest(ws)
	assert.False(t, ok, "a dry-run must not write a manifest")

	// nil snapshot (persistent workspace): nothing written.
	require.NoError(t, writeSnapshotManifest(ws, nil, false))
	_, ok = readWorkspaceManifest(ws)
	assert.False(t, ok, "the persistent workspace gets no manifest")
}
