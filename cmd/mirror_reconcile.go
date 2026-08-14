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
		if isGitClone(filepath.Join(ownerDir, repo.Name)) {
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

// toReport maps the internal counts onto the JSON reconciliation surface (issue
// #48 WS3). notOnDisk is the shortfall (the honest "failed to land" count); the
// branch tallies are attached only when a --branch was requested, so a no-branch
// mirror's report carries no branch object rather than a block of ambiguous zeros.
func (r reconciliation) toReport() mirrorReconciliation {
	out := mirrorReconciliation{
		InSelection: r.inSelection,
		OnDisk:      r.onDisk,
		NotOnDisk:   r.shortfall(),
	}
	if r.hasBranch {
		out.Branch = &mirrorBranchReconciliation{
			Present:  r.branchPresent,
			FellBack: r.fellBack,
			Unknown:  r.unknown,
		}
	}
	return out
}

// reportReconciliation prints goldfinger's authoritative post-mirror summary to
// errOut (stderr — stdout stays reserved for the path/JSON) from a precomputed
// reconciliation, so the caller can share the one filesystem stat with the JSON
// report. A full mirror reads as a success line; a shortfall is flagged as a
// warning pointing at the captured ghorg log (logPath, "" if none) for the
// underlying clone errors.
func reportReconciliation(errOut io.Writer, rec reconciliation, ws, owner, logPath string) {
	if n := rec.shortfall(); n > 0 {
		hint := "re-run mirror, or check ghorg's output above for clone errors"
		if logPath != "" {
			hint = "re-run mirror, or check the captured ghorg log for clone errors: " + logPath
		}
		warn(errOut, "reconciliation: "+rec.line())
		warn(errOut, fmt.Sprintf("%d selected repo(s) are not on disk under %s/%s — the mirror under-covered the selection; %s", n, ws, owner, hint))
		return
	}
	done(errOut, "reconciliation: "+rec.line())
}

// isGitClone reports whether path is an actual git clone — a directory that
// holds a .git entry — rather than a bare, leftover, or half-written directory.
// reconcile counts a repo "on disk" only when this holds, so a stale directory
// from an earlier interrupted mirror (or any unrelated dir a name happens to
// match) can't be miscounted as covered on the one line marketed as the
// authoritative coverage truth. It stays within the charter: a read-only stat,
// never git. The .git entry is accepted whether it is a directory (a normal
// ghorg clone) or a file (a gitfile-linked worktree), so "is this a clone" is
// answered honestly without assuming ghorg's exact layout.
//
// The root must be a REAL directory, tested with Lstat: a symlink at the clone
// location — even one pointing at a valid git tree — is rejected. ghorg never
// creates one, so its only source is tampering or an unrelated symlink a repo name
// happens to match; counting it as covered would let reconcile claim coverage of a
// tree it never actually walked through the workspace. (scan makes the same "is
// this a real clone" judgement independently, via a workspace-confined os.Root
// handle — see openScanClone.) Rejecting it keeps the report honest.
func isGitClone(path string) bool {
	if !isRealDir(path) {
		return false
	}
	// Lstat (not Stat) the .git entry too: a real clone's .git is a directory
	// (normal ghorg clone) or a regular file (gitfile-linked worktree). A .git
	// SYMLINK is never something ghorg creates, so its only source is tampering —
	// following it (os.Stat would) could count an unrelated git tree as this repo's
	// clone and report false coverage. Accept only a real dir or regular file.
	fi, err := os.Lstat(filepath.Join(path, ".git"))
	if err != nil {
		return false
	}
	return fi.IsDir() || fi.Mode().IsRegular()
}

// isRealDir reports whether path exists and is a directory that is NOT a symlink
// (Lstat describes the entry itself, so a symlink to a directory reports
// ModeSymlink and fails here).
func isRealDir(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.IsDir()
}
