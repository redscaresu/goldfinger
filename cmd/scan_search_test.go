package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeFile writes content to <dir>/<rel>, creating parent dirs.
func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
}

// searchDir opens dir as a confined root (as computeScanReport does per clone) and
// runs searchTree over it, so the unit tests exercise the same handle-pinned walk
// the command uses.
func searchDir(t *testing.T, dir string, re *regexp.Regexp) repoScan {
	t.Helper()
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()
	res, err := searchTree(root, re)
	require.NoError(t, err)
	return res
}

func TestSearchTreeFindsMatchesWithRepoRelativePaths(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Dockerfile", "FROM debian:bullseye\nRUN true\n")
	writeFile(t, dir, "sub/app.txt", "no match here\ndebian:bullseye again\n")
	writeFile(t, dir, "clean.txt", "nothing to see\n")

	res := searchDir(t, dir, regexp.MustCompile(`debian:bullseye`))
	require.False(t, res.truncated)
	require.Len(t, res.matches, 2)

	// Sorted by path then line: Dockerfile:1 before sub/app.txt:2.
	assert.Equal(t, "Dockerfile", res.matches[0].Path)
	assert.Equal(t, 1, res.matches[0].Line)
	assert.Equal(t, "FROM debian:bullseye", res.matches[0].Text)
	assert.Equal(t, "sub/app.txt", res.matches[1].Path, "path must be repo-relative with forward slashes")
	assert.Equal(t, 2, res.matches[1].Line)
}

func TestSearchTreeSkipsGitDirectory(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".git/config", "url = debian:bullseye\n")
	writeFile(t, dir, "README.md", "debian:bullseye\n")

	res := searchDir(t, dir, regexp.MustCompile(`debian:bullseye`))
	require.Len(t, res.matches, 1)
	assert.Equal(t, "README.md", res.matches[0].Path, ".git internals must never be searched")
}

func TestSearchTreeSkipsBinaryFiles(t *testing.T) {
	dir := t.TempDir()
	// A NUL byte marks the file binary; grep's default skips it.
	writeFile(t, dir, "blob.bin", "match\x00match\n")
	writeFile(t, dir, "text.txt", "match\n")

	res := searchDir(t, dir, regexp.MustCompile(`match`))
	require.Len(t, res.matches, 1)
	assert.Equal(t, "text.txt", res.matches[0].Path)
	assert.False(t, res.truncated, "a skipped binary is not a truncation of the text search")
}

func TestSearchTreeSkipsSymlinks(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	require.NoError(t, os.WriteFile(outside, []byte("match\n"), 0o644))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "link.txt")))
	writeFile(t, dir, "real.txt", "match\n")

	res := searchDir(t, dir, regexp.MustCompile(`match`))
	require.Len(t, res.matches, 1)
	assert.Equal(t, "real.txt", res.matches[0].Path, "a symlink must never lead the read out of the mirrored tree")
}

func TestSearchTreeMarksTruncatedOnUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permission bits, so an unreadable file cannot be simulated")
	}
	dir := t.TempDir()
	writeFile(t, dir, "readable.txt", "match\n")
	writeFile(t, dir, "locked.txt", "match\n")
	locked := filepath.Join(dir, "locked.txt")
	require.NoError(t, os.Chmod(locked, 0o000))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) }) // let TempDir cleanup remove it

	// The readable file still matches, but the file we could not open is a real gap
	// in coverage, so the scan is flagged truncated rather than silently dropping it.
	res := searchDir(t, dir, regexp.MustCompile(`match`))
	require.Len(t, res.matches, 1)
	assert.Equal(t, "readable.txt", res.matches[0].Path)
	assert.True(t, res.truncated, "an unreadable file is a coverage gap and must flag truncated")
}

// TestSearchFileRefusesEscapingSymlink locks the os.Root confinement directly: even
// if searchFile is handed a relative path that resolves through a symlink escaping
// the pinned repo root (the walk→read TOCTOU an attacker would race), root.Open
// refuses it — no content outside the tree is read — and the miss is flagged
// truncated.
func TestSearchFileRefusesEscapingSymlink(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret.txt")
	require.NoError(t, os.WriteFile(secret, []byte("TOPSECRET\n"), 0o644))
	require.NoError(t, os.Symlink(secret, filepath.Join(dir, "link.txt")))

	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()

	var res repoScan
	searchFile(root, "link.txt", regexp.MustCompile(`TOPSECRET`), &res)
	assert.Empty(t, res.matches, "an escaping symlink must never lead the read out of the tree")
	assert.True(t, res.truncated, "a refused escaping symlink is a coverage gap flagged truncated")
}

func TestSearchTreeSkipsOversizeFileAndMarksTruncated(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("a", maxFileBytes+1)
	writeFile(t, dir, "big.txt", big+"\nmatchword\n")
	writeFile(t, dir, "small.txt", "matchword\n")

	res := searchDir(t, dir, regexp.MustCompile(`matchword`))
	require.Len(t, res.matches, 1)
	assert.Equal(t, "small.txt", res.matches[0].Path)
	assert.True(t, res.truncated, "skipping an oversize file must flag the scan truncated")
}

func TestSearchTreeCapsMatchesPerRepo(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := 0; i < maxMatchesPerRepo+50; i++ {
		b.WriteString("hit\n")
	}
	writeFile(t, dir, "many.txt", b.String())

	res := searchDir(t, dir, regexp.MustCompile(`hit`))
	assert.Len(t, res.matches, maxMatchesPerRepo, "match list is capped per repo")
	assert.True(t, res.truncated, "hitting the per-repo cap must flag truncated")
}

func TestSearchTreeHandlesCRLFAndTrimsCarriageReturn(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "win.txt", "hello world\r\nsecond\r\n")

	res := searchDir(t, dir, regexp.MustCompile(`hello`))
	require.Len(t, res.matches, 1)
	assert.Equal(t, "hello world", res.matches[0].Text, "trailing CR is trimmed from echoed text")
}

func TestSearchTreeTrimsLongMatchText(t *testing.T) {
	dir := t.TempDir()
	long := "MATCH" + strings.Repeat("x", maxMatchTextBytes*2)
	writeFile(t, dir, "min.js", long+"\n")

	res := searchDir(t, dir, regexp.MustCompile(`MATCH`))
	require.Len(t, res.matches, 1)
	assert.True(t, strings.HasSuffix(res.matches[0].Text, truncationMarker), "over-long line is trimmed with a marker")
	assert.LessOrEqual(t, len(res.matches[0].Text), maxMatchTextBytes+len(truncationMarker))
}

// TestSearchTreeErrorsWhenRootUnreadable proves a clone whose root cannot be LISTED
// is surfaced as an error, not a silent clean scan. Root.FS() re-opens the directory
// to read its entries, so making the dir unreadable after the handle is pinned still
// fails the walk's root ReadDir — the whole tree is a coverage gap, and searchTree
// must return an error so the caller reports the repo not-scanned (issue #28), rather
// than scanned/0-match.
func TestSearchTreeErrorsWhenRootUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permission bits, so an unreadable dir cannot be simulated")
	}
	dir := t.TempDir()
	writeFile(t, dir, "x.txt", "hit\n")
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()
	require.NoError(t, os.Chmod(dir, 0o000))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	res, err := searchTree(root, regexp.MustCompile(`hit`))
	require.Error(t, err, "an unreadable clone root must be an error, not a silent clean scan")
	assert.Empty(t, res.matches)
}

// TestSearchTreeSkipsGitFileWorktree guards the .git-as-a-FILE case: a
// gitfile-linked worktree stores its .git as a regular file (pointing at the real
// gitdir), not a directory. It is still VCS internals, so searchTree must skip it
// before the regular-file read — otherwise the internal gitdir path would leak into
// the report.
func TestSearchTreeSkipsGitFileWorktree(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".git", "gitdir: /elsewhere/match\n") // a .git FILE, not a directory
	writeFile(t, dir, "README.md", "match\n")

	res := searchDir(t, dir, regexp.MustCompile(`match`))
	require.Len(t, res.matches, 1)
	assert.Equal(t, "README.md", res.matches[0].Path, "a .git gitfile is VCS internal and must not be searched")
}

func TestCompileScanPattern(t *testing.T) {
	t.Run("regex default", func(t *testing.T) {
		re, err := compileScanPattern("deb.an", false, false)
		require.NoError(t, err)
		assert.True(t, re.MatchString("debian"))
		assert.True(t, re.MatchString("deb0an"))
	})
	t.Run("fixed strings escapes metachars", func(t *testing.T) {
		re, err := compileScanPattern("deb.an", false, true)
		require.NoError(t, err)
		assert.True(t, re.MatchString("deb.an"))
		assert.False(t, re.MatchString("debian"), "-F treats . as a literal dot")
	})
	t.Run("ignore case", func(t *testing.T) {
		re, err := compileScanPattern("Debian", true, false)
		require.NoError(t, err)
		assert.True(t, re.MatchString("DEBIAN"))
	})
	t.Run("fixed + ignore case", func(t *testing.T) {
		re, err := compileScanPattern("A.B", true, true)
		require.NoError(t, err)
		assert.True(t, re.MatchString("a.b"))
		assert.False(t, re.MatchString("axb"))
	})
	t.Run("invalid regex errors", func(t *testing.T) {
		_, err := compileScanPattern("(unclosed", false, false)
		assert.Error(t, err)
	})
	t.Run("empty pattern errors", func(t *testing.T) {
		_, err := compileScanPattern("", false, false)
		require.Error(t, err, "an empty pattern matches every line — rejected as a mistake")
		assert.Contains(t, err.Error(), "empty")
	})
}

// TestSearchTreeNoPhantomLineForTrailingNewline guards the line iterator: a file
// ending in '\n' has N lines, not N+1, so an empty-matching pattern reports only
// the lines that exist (grep semantics).
func TestSearchTreeNoPhantomLineForTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "two.txt", "alpha\nbeta\n") // two lines, trailing newline
	writeFile(t, dir, "empty.txt", "")            // zero lines

	// `.` matches any line with at least one character; it must NOT match a phantom
	// empty final line after the trailing newline, nor anything in the empty file.
	res := searchDir(t, dir, regexp.MustCompile(`.`))
	require.Len(t, res.matches, 2, "trailing newline must not add a phantom empty line")
	assert.Equal(t, "two.txt", res.matches[0].Path)
	assert.Equal(t, 1, res.matches[0].Line)
	assert.Equal(t, "beta", res.matches[1].Text)
	assert.Equal(t, 2, res.matches[1].Line)
}
