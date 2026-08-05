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
// knowable from the lockfile and the resolved options alone: goldfinger runs no
// git and re-runs no discovery to build it.
type mirrorReport struct {
	Version         int              `json:"version"`
	Workspace       string           `json:"workspace"`
	Owner           string           `json:"owner"`
	RepoCount       int              `json:"repoCount"`
	Branch          string           `json:"branch,omitempty"`
	BranchFactsNote string           `json:"branchFactsNote,omitempty"`
	Repos           []mirrorRepoInfo `json:"repos"`
}

// mirrorRepoInfo is the per-repo slice of the report.
type mirrorRepoInfo struct {
	Repo          string `json:"repo"` // owner/name
	DefaultBranch string `json:"defaultBranch"`
	BranchStatus  string `json:"branchStatus"`
}

// buildMirrorReport assembles the report from the selection, the resolved
// workspace, and the mirror options. It is pure — no I/O — so it is trivially
// testable and provably free of git/discovery calls.
func buildMirrorReport(sel models.Selection, ws string, opts mirror.Options) mirrorReport {
	rep := mirrorReport{
		Version:   mirrorReportVersion,
		Workspace: ws,
		Owner:     sel.Owner,
		RepoCount: len(sel.Repos),
		Branch:    opts.Branch,
		Repos:     make([]mirrorRepoInfo, 0, len(sel.Repos)),
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
