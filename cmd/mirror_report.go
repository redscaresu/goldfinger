package main

import (
	"github.com/redscaresu/goldfinger/mirror"
	"github.com/redscaresu/goldfinger/models"
)

// branchStatus categorises, for the branch a mirror requested, what the
// lockfile knows about each repo. Values are derived purely from facts recorded
// at selection time (never git, never re-discovery).
const (
	// branchStatusHas: the requested branch was present at selection time (or is
	// the repo's own default), so ghorg checks it out.
	branchStatusHas = "has-branch"
	// branchStatusFallback: the branch was absent at selection time, so ghorg
	// leaves this repo on its default branch.
	branchStatusFallback = "falls-back-to-default"
	// branchStatusUnknown: the branch was never checked at selection time (an old
	// v1 lockfile, or `select --branch-presence` was not run for it) — goldfinger
	// does not guess.
	branchStatusUnknown = "unknown"
	// branchStatusDefault: no --branch was requested, so every repo is mirrored on
	// its own default branch and presence is not a question.
	branchStatusDefault = "default-branch"
)

// branchFactsNote explains that the report's branch categorisation is only as
// current as the selection. It is emitted whenever a --branch was requested.
const branchFactsNote = "branchStatus values come from branch presence recorded at selection time (via `select --branch-presence`) and can drift; \"unknown\" means the branch was not checked then — goldfinger does not guess it here."

// mirrorReport is the machine-readable summary of a mirror run. Every field is
// knowable without git or re-running discovery: the selection-derived fields
// come from the lockfile and the resolved options, and `reconciliation` from a
// read-only filesystem check (a directory stat per repo) — the aggregate WS3
// (issue #48) makes goldfinger's own coverage/failure truth the default machine
// surface instead of ghorg's pass-through stream.
type mirrorReport struct {
	Version         int                  `json:"version"`
	Workspace       string               `json:"workspace"`
	Owner           string               `json:"owner"`
	RepoCount       int                  `json:"repoCount"`
	Branch          string               `json:"branch,omitempty"`
	BranchFactsNote string               `json:"branchFactsNote,omitempty"`
	Reconciliation  mirrorReconciliation `json:"reconciliation"`
	Repos           []mirrorRepoInfo     `json:"repos"`
}

// mirrorReconciliation is the aggregate coverage count for a completed mirror
// (issue #48 WS3): how many selected repos actually landed on disk, and how many
// did not (`notOnDisk` — the honest counterpart to a "failed" count, derived from
// a read-only stat, not from parsing ghorg or running git). goldfinger cannot
// split on-disk repos into "freshly cloned" vs "already present" without running
// git, so it deliberately reports neither — only the coverage that is provable.
type mirrorReconciliation struct {
	InSelection int `json:"inSelection"`
	OnDisk      int `json:"onDisk"`
	NotOnDisk   int `json:"notOnDisk"`
	// Branch is present only when a --branch was requested, so the tallies (which
	// then always appear together) can't be mistaken for a no-branch mirror's
	// zeros. It comes from branch presence frozen at selection time (can drift).
	Branch *mirrorBranchReconciliation `json:"branch,omitempty"`
}

// mirrorBranchReconciliation tallies, for a requested --branch, how the selected
// repos split by the presence recorded at selection time: present (ghorg checks
// the branch out), fellBack (absent → ghorg leaves the repo on its default), and
// unknown (never checked at selection time — goldfinger does not guess).
type mirrorBranchReconciliation struct {
	Present  int `json:"present"`
	FellBack int `json:"fellBack"`
	Unknown  int `json:"unknown"`
}

// mirrorRepoInfo is the per-repo slice of the report.
type mirrorRepoInfo struct {
	Repo          string `json:"repo"` // owner/name
	DefaultBranch string `json:"defaultBranch"`
	BranchStatus  string `json:"branchStatus"`
}

// buildMirrorReport assembles the report from the selection, the resolved
// workspace, the mirror options, and the precomputed reconciliation. It is pure
// — no I/O — so it is trivially testable and provably free of git/discovery
// calls; the caller does the one read-only stat (reconcile) and passes the
// result in, keeping the filesystem read out of this builder.
func buildMirrorReport(sel models.Selection, ws string, opts mirror.Options, rec reconciliation) mirrorReport {
	rep := mirrorReport{
		Version:        mirrorReportVersion,
		Workspace:      ws,
		Owner:          sel.Owner,
		RepoCount:      len(sel.Repos),
		Branch:         opts.Branch,
		Reconciliation: rec.toReport(),
		Repos:          make([]mirrorRepoInfo, 0, len(sel.Repos)),
	}
	if opts.Branch != "" {
		rep.BranchFactsNote = branchFactsNote
	}
	for _, r := range sel.Repos {
		rep.Repos = append(rep.Repos, mirrorRepoInfo{
			Repo:          r.FullName(),
			DefaultBranch: r.DefaultBranch,
			BranchStatus:  branchStatusFor(r, opts.Branch),
		})
	}
	return rep
}

// branchStatusFor categorises one repo against the requested branch.
func branchStatusFor(r models.Repo, branch string) string {
	if branch == "" {
		return branchStatusDefault
	}
	has, known := r.RecordedBranch(branch)
	switch {
	case !known:
		return branchStatusUnknown
	case has:
		return branchStatusHas
	default:
		return branchStatusFallback
	}
}
