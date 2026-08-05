package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/redscaresu/goldfinger/mirror"
	"github.com/redscaresu/goldfinger/models"
)

// reconciliation is goldfinger's own count-based truth about a completed mirror,
// derived from the two sources it can read without running git or re-running
// discovery: the lockfile (what was selected, and — for a requested branch —
// what presence was frozen at selection time) and a read-only filesystem check
// (how many of those repos actually landed on disk). It exists because ghorg's
// own "N new clones" line counts only *newly* created clones — a re-mirror of an
// unchanged fleet reports 0, which reads as "nothing there" — and because ghorg
// prints one "Could not checkout <branch>" line per fall-back repo, noise that
// scans as failure. This single line is the authoritative counterpart to both.
type reconciliation struct {
	inSelection   int
	onDisk        int
	hasBranch     bool // a --branch was requested, so the branch tallies below are meaningful
	branchPresent int  // requested branch recorded present at selection time
	fellBack      int  // requested branch recorded absent -> ghorg leaves the repo default
	unknown       int  // requested branch never checked at selection time
}

// reconcile counts the selection against what is on disk under <ws>/<owner>. The
// on-disk check is a plain directory stat per repo — read-only, never git — and
// the branch tallies come straight from the lockfile facts via branchStatusFor,
// so this stays within the charter (no git, no re-discovery).
func reconcile(sel models.Selection, ws string, opts mirror.Options) reconciliation {
	r := reconciliation{inSelection: len(sel.Repos), hasBranch: opts.Branch != ""}
	ownerDir := filepath.Join(ws, sel.Owner)
	for _, repo := range sel.Repos {
		if isDir(filepath.Join(ownerDir, repo.Name)) {
			r.onDisk++
		}
		if !r.hasBranch {
			continue
		}
		switch branchStatusFor(repo, opts.Branch) {
		case branchStatusHas:
			r.branchPresent++
		case branchStatusFallback:
			r.fellBack++
		case branchStatusUnknown:
			r.unknown++
		}
	}
	return r
}

// line renders the human reconciliation summary, e.g.
//
//	in selection: 59 | on disk: 59 | branch present: 15 | fell back: 44
//
// The branch tallies appear only when a --branch was requested; "unknown" is
// shown only when non-zero, so the common fully-recorded case stays terse and
// the tallies shown always sum to the selection count.
func (r reconciliation) line() string {
	s := fmt.Sprintf("in selection: %d | on disk: %d", r.inSelection, r.onDisk)
	if r.hasBranch {
		s += fmt.Sprintf(" | branch present: %d | fell back: %d", r.branchPresent, r.fellBack)
		if r.unknown > 0 {
			s += fmt.Sprintf(" | unknown: %d", r.unknown)
		}
	}
	return s
}

// shortfall is the number of selected repos that did not land on disk. A
// positive value means the mirror under-covered the selection — a real problem,
// distinct from a branch fall-back (which is expected and not a coverage gap).
func (r reconciliation) shortfall() int {
	return r.inSelection - r.onDisk
}

// reportReconciliation prints goldfinger's authoritative post-mirror summary to
// errOut (stderr — stdout stays reserved for the path/JSON). It is skipped on a
// dry-run, which clones nothing, so an on-disk count of 0 would be misleading.
// A full mirror reads as a success line; a shortfall is flagged as a warning
// pointing back at ghorg's own output for the underlying clone errors.
func reportReconciliation(errOut io.Writer, sel models.Selection, ws string, opts mirror.Options) {
	if opts.DryRun {
		return
	}
	rec := reconcile(sel, ws, opts)
	if n := rec.shortfall(); n > 0 {
		warn(errOut, "reconciliation: "+rec.line())
		warn(errOut, fmt.Sprintf("%d selected repo(s) are not on disk under %s/%s — the mirror under-covered the selection; re-run mirror, or check ghorg's output above for clone errors", n, ws, sel.Owner))
		return
	}
	done(errOut, "reconciliation: "+rec.line())
}

// isDir reports whether path exists and is a directory.
func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
