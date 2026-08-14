package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// scan caps. These bound the work and the output so a scan over a large fleet
// can never blow up memory or emit an unusably huge report. When a cap trims what
// would otherwise be searched or reported, the scan sets a `truncated` flag rather
// than silently dropping the fact (issue #28: no silent truncation).
const (
	// maxFileBytes: files larger than this are not read. Source, manifests, and
	// lockfiles are comfortably under 10 MiB; anything bigger is almost certainly a
	// data blob, not something a text audit greps. A skipped-oversize file marks the
	// scan truncated (it could, in principle, have held a match).
	maxFileBytes = 10 << 20 // 10 MiB
	// maxMatchesPerRepo bounds a single repo's match list so one pathological repo
	// (a generated file that matches on every line) can't dominate the report.
	// Hitting it marks that repo — and the whole scan — truncated.
	maxMatchesPerRepo = 1000
	// maxMatchTextBytes caps the stored `text` of a single match so a match on a
	// minified/one-line file does not embed megabytes in the JSON. The match is
	// still counted and its line recorded; only the echoed text is trimmed, with a
	// trailing marker. It does not set truncated: the match itself is fully
	// reported, only its display text is shortened.
	maxMatchTextBytes = 512
)

// truncationMarker is appended to a match's text when it was trimmed to
// maxMatchTextBytes, so a reader can tell the echoed line is not the whole line.
const truncationMarker = "…[truncated]"

// scanMatch is one matching line within a repo. Path is repo-relative (forward
// slashes), Line is 1-based, Text is the matching line (trimmed to
// maxMatchTextBytes).
type scanMatch struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// repoScan is the pure result of searching one on-disk repo tree: its matches
// plus whether a cap trimmed the search (a match-cap hit or an oversize file
// skipped). It carries no notion of the selection or the report — cmd maps it into
// the per-repo report slice.
type repoScan struct {
	matches   []scanMatch
	truncated bool
}

// searchTree walks the repo clone pinned by root and returns every line matching
// re, as repo-relative matches. It is pure filesystem work — no git, no network —
// and deliberately conservative about what it reads:
//
//   - the .git entry is skipped wholesale, whether it is a directory (a normal
//     clone) or a regular file (a gitfile-linked worktree): VCS internals are not
//     repo content, and a searchable gitfile would leak its internal gitdir path;
//   - only entries WalkDir reports as regular files are read; symlinks, devices, and
//     sockets are skipped. A statically-present symlink is therefore never followed.
//     (An entry actively swapped regular→symlink in the walk→open window could be
//     followed, but only WITHIN the repo root — os.Root confines every read to the
//     mirrored tree, so a read can never escape it; see searchFile.);
//   - files over maxFileBytes are skipped (and mark the result truncated);
//   - files containing a NUL byte are treated as binary and skipped (grep's default);
//   - the match list is capped at maxMatchesPerRepo (hitting it marks truncated).
//
// root is a directory handle the caller opened, confined WITHIN the workspace: it
// pins a descriptor on this exact clone, so every ReadDir and Open below resolves
// against THAT directory — immune to the path being swapped for a symlink after
// this point (the walk→read TOCTOU) — and io/fs refuses any symlink component that
// would escape the root. That makes "a symlink can never lead the read out of the
// mirrored tree" a property of the handle, not a per-file best-effort, and it is
// portable (no O_NOFOLLOW), so it compiles on every target.
//
// A walk error on an INDIVIDUAL entry (an unreadable file or subtree) is not fatal
// — one bad file must not abort the audit of an otherwise readable repo — but it IS
// a real gap in coverage, so it marks the result truncated rather than being
// silently dropped. A walk error on the ROOT itself is different: nothing was
// searched, so searchTree returns an error and the caller reports the repo
// not-scanned rather than a misleading scanned/0-match result. This matters because
// Root.FS() re-opens the directory to list it, so the root read can fail AFTER the
// caller's on-disk gate opened the handle (a permission/identity race) — a gap we
// must surface, never swallow (issue #28: no silent truncation).
func searchTree(root *os.Root, re *regexp.Regexp) (repoScan, error) {
	var res repoScan
	var rootErr error
	_ = fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			if rel == "." {
				// The clone root itself could not be read (Root.FS() re-opens it to list
				// its entries, so this can fail even after the caller opened the handle).
				// We searched NOTHING here, so record the failure and abort the walk; the
				// caller classifies the repo not-scanned rather than scanned/0.
				rootErr = err
				return err
			}
			// A deeper unreadable file or subtree is content we could not search: skip it
			// (and its subtree if it is a dir) rather than abort the whole repo scan, but
			// flag the result truncated — never claim coverage we lack.
			res.truncated = true
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		// Skip the VCS internals wholesale, BEFORE the dir/file split: .git is usually
		// a directory (normal clone) but is a regular file for a gitfile-linked
		// worktree — searching that file would spill the worktree's internal gitdir
		// path into the report, so skip either shape.
		if d.Name() == ".git" {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		// Only regular files: skip symlinks (which could point outside the tree),
		// devices, sockets, and named pipes.
		if !d.Type().IsRegular() {
			return nil
		}
		if res.matchCount() >= maxMatchesPerRepo {
			// Already at the cap; no point reading more files. Mark truncated and stop.
			res.truncated = true
			return fs.SkipAll
		}
		// rel is a slash-separated path relative to the pinned root (io/fs convention).
		searchFile(root, rel, re, &res)
		return nil
	})
	if rootErr != nil {
		return res, fmt.Errorf("read clone root: %w", rootErr)
	}
	sort.Slice(res.matches, func(i, j int) bool {
		if res.matches[i].Path != res.matches[j].Path {
			return res.matches[i].Path < res.matches[j].Path
		}
		return res.matches[i].Line < res.matches[j].Line
	})
	return res, nil
}

// matchCount reports how many matches have been collected so far.
func (r *repoScan) matchCount() int { return len(r.matches) }

// searchFile reads one regular file (rel, a slash path relative to the pinned repo
// root handle) and appends its matching lines to res. It enforces the size cap,
// binary-skip, and per-repo match cap, setting res.truncated when a cap or an
// unreadable file trims what it would otherwise have searched.
//
// The file is opened via root.Open (os.Root), which resolves rel WITHIN the pinned
// root and refuses any symlink component that would escape it. That closes the
// walk→read TOCTOU window for real (an entry swapped to an escaping symlink after
// WalkDir saw it as a regular file is refused, not followed) and makes the "a
// symlink can never lead the read out of the mirrored tree" guarantee a property of
// the handle, not a best-effort Lstat race. A file we cannot open or stat is a real
// coverage gap, so it marks the scan truncated rather than being silently dropped.
func searchFile(root *os.Root, rel string, re *regexp.Regexp, res *repoScan) {
	f, err := root.Open(rel) //nolint:gosec // G304: rel is a repo-relative entry WalkDir found under root, and root.Open confines the read within the pinned mirror clone — reading the repo content goldfinger was pointed at is the whole purpose of scan.
	if err != nil {
		// Unreadable, or a symlink that would escape the mirror tree (root.Open
		// refuses it): a gap in coverage, not a silent skip.
		res.truncated = true
		return
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		res.truncated = true
		return
	}
	// Stat on the open descriptor: after OpenInRoot has confined the open, a
	// non-regular result (a device or fifo the swap couldn't be) is simply not
	// searchable text — skip it, but it is not a coverage gap of the audit's target.
	if !fi.Mode().IsRegular() {
		return
	}
	if fi.Size() > maxFileBytes {
		// An oversize file is a real gap in coverage — flag it so the report never
		// implies the whole tree was searched.
		res.truncated = true
		return
	}
	// Read at most maxFileBytes+1: the fstat size is advisory (the file could grow
	// between stat and read), so cap the read at the descriptor and treat an
	// over-cap read as an oversize skip rather than buffering an unbounded file.
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		res.truncated = true
		return
	}
	if int64(len(data)) > maxFileBytes {
		res.truncated = true
		return
	}
	// Binary heuristic: a NUL byte in the content. Matches grep's default of not
	// searching binary files; not treated as a coverage gap (binaries are not the
	// target of a text audit), so it does not set truncated.
	if bytes.IndexByte(data, 0) >= 0 {
		return
	}
	appendMatches(data, rel, re, res)
}

// appendMatches runs re over each line of data and appends the matches to res,
// stopping at the per-repo cap. It scans line by line with IndexByte rather than
// bytes.Split so it never materializes a slice header per line (a 10 MiB
// newline-heavy file would otherwise allocate hundreds of MiB) and can stop at
// the cap without having split the whole file. A trailing '\r' is trimmed so both
// LF and CRLF files report clean text. A trailing newline does NOT yield a
// phantom empty final line, and an empty file yields no lines — matching grep, so
// an empty-matching pattern (^, .*) can't invent non-existent lines. Matching is
// against the full line (RE2 is linear-time, so even a very long minified line is
// safe); only the stored text is trimmed to maxMatchTextBytes. rel arrives in the
// OS-native form searchFile opened it with; it is converted to forward slashes once
// here so a match's reported Path is stable across platforms.
func appendMatches(data []byte, rel string, re *regexp.Regexp, res *repoScan) {
	slashRel := filepath.ToSlash(rel)
	line := 0
	for len(data) > 0 {
		if res.matchCount() >= maxMatchesPerRepo {
			res.truncated = true
			return
		}
		line++
		var raw []byte
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			raw, data = data[:i], data[i+1:]
		} else {
			raw, data = data, nil
		}
		text := bytes.TrimSuffix(raw, []byte("\r"))
		if !re.Match(text) {
			continue
		}
		res.matches = append(res.matches, scanMatch{
			Path: slashRel,
			Line: line,
			Text: trimMatchText(text),
		})
	}
}

// trimMatchText renders a matching line as display text, trimmed to
// maxMatchTextBytes with a marker when it was longer. It trims on a rune boundary
// so the echoed text stays valid UTF-8.
func trimMatchText(b []byte) string {
	s := string(b)
	if len(s) <= maxMatchTextBytes {
		return s
	}
	cut := maxMatchTextBytes
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimRight(s[:cut], "\r\n") + truncationMarker
}

// utf8RuneStart reports whether b is the first byte of a UTF-8 rune (i.e. not a
// 0b10xxxxxx continuation byte), so trimMatchText never splits a multi-byte rune.
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// compileScanPattern builds the search regexp from the raw pattern and the two
// interpretation flags. fixedStrings escapes every regex metacharacter (a literal
// search); ignoreCase prepends the case-insensitive flag. An empty pattern, or one
// that fails to compile, is a caller-facing error.
func compileScanPattern(pattern string, ignoreCase, fixedStrings bool) (*regexp.Regexp, error) {
	if pattern == "" {
		// An empty pattern compiles fine but matches every line — never what an
		// audit wants, and almost always a mistake (a shell that dropped the arg).
		return nil, errors.New("pattern must not be empty")
	}
	expr := pattern
	if fixedStrings {
		expr = regexp.QuoteMeta(expr)
	}
	if ignoreCase {
		expr = "(?i)" + expr
	}
	return regexp.Compile(expr)
}
