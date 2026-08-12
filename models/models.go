// Package models holds the domain types shared across goldfinger's packages.
// It has no dependencies on other goldfinger packages.
package models

import "time"

// Repo is a GitHub repository in a selection.
type Repo struct {
	Owner         string   `json:"owner"`
	Name          string   `json:"name"`
	CloneURL      string   `json:"cloneURL"`
	DefaultBranch string   `json:"defaultBranch"`
	Topics        []string `json:"topics,omitempty"`
	Archived      bool     `json:"archived,omitempty"`

	// BranchPresence records, per branch name checked at selection time via
	// read-only REST (`select --branch-presence`), whether that branch existed on
	// this repo. It is a frozen fact recorded at selection time and can drift
	// afterwards; an absent entry means the branch was never checked, so callers
	// must treat it as "unknown" and never guess.
	BranchPresence map[string]bool `json:"branchPresence,omitempty"`
}

// FullName returns the canonical "owner/name" identifier.
func (r Repo) FullName() string {
	return r.Owner + "/" + r.Name
}

// RecordedBranch reports what the selection knows about branch on this repo.
// known is false when the branch was not checked at selection time (an old v1
// lockfile, or a branch never passed to `select --branch-presence`) — callers
// must not guess in that case. has is whether the branch was present. A branch
// equal to the repo's own DefaultBranch is always present and known without any
// recorded fact.
func (r Repo) RecordedBranch(branch string) (has, known bool) {
	if branch != "" && branch == r.DefaultBranch {
		return true, true
	}
	has, known = r.BranchPresence[branch]
	return has, known
}

// TokenEnvVar is the environment variable goldfinger reads the operator's
// GitHub PAT from. goldfinger maps it onto each child tool's own token variable
// (GITHUB_TOKEN, GHORG_GITHUB_TOKEN) and strips it from the child environment,
// so the raw PAT never reaches a delegate or a user-supplied apply script under
// this name.
const TokenEnvVar = "GOLD_FINGER_PAT"

// Owner types as reported by the GitHub API and stored in a Selection.
const (
	OwnerUser         = "User"
	OwnerOrganization = "Organization"
)

// SelectionFilter records how a selection was resolved, for provenance.
type SelectionFilter struct {
	AllRepos bool     `json:"allRepos"`
	Topics   []string `json:"topics,omitempty"`

	// Repos, when non-empty, marks an EXPLICIT selection: the operator named an
	// exact set of repo basenames (`select --repo` / `--repos-from`) rather than
	// resolving a topic/all-repos filter. It is the explicit-mode marker `check`
	// keys on — for such a selection, drift is the frozen set diffed against live
	// existence, not a re-run of a filter (which would match nothing and report
	// every repo as removed). Additive and omitempty, so an older reader ignores
	// it and a filtered selection omits it entirely.
	Repos []string `json:"repos,omitempty"`
}

// SelectionVersion is the current lockfile schema version. v2 added per-repo
// branch-presence facts (Repo.BranchPresence) and the list of branch names
// checked at selection time (Selection.BranchesChecked); v1 lockfiles carry
// neither and read back with "unknown" branch facts.
const SelectionVersion = 2

// Signing modes for a real apply run. There is no default: a real run must
// state its signing intent explicitly, because commit provenance is not a safe
// thing to leave implicit for an outward-facing, hard-to-reverse action.
const (
	// SignLocal maps to multi-gitter --git-type=cmd: the real git binary runs the
	// commit, so the operator's ~/.gitconfig (commit.gpgsign / user.signingkey)
	// applies and commits are signed with their own GPG key.
	SignLocal = "local"
	// SignGitHub maps to multi-gitter --api-push: commits go through the GitHub
	// API and are signed by GitHub's web-flow key (always "Verified").
	SignGitHub = "github"
	// SignNone applies no signing flag: multi-gitter's default go-git path, which
	// produces unsigned commits.
	SignNone = "none"
)

// validSignModes is the canonical set of accepted signing modes and the single
// source of truth: cmd's --sign validator and the guide --json catalogue (via
// SignModes) and apply.Apply's execution-boundary guard (via IsValidSignMode)
// all derive from it, so no layer can drift from another. It is unexported so no
// other package can mutate the list the safety guard trusts — an exported slice
// could be appended to (e.g. add "") to defeat the check. There is deliberately
// no default: a run must name a mode.
var validSignModes = []string{SignLocal, SignGitHub, SignNone}

// SignModes returns a fresh copy of the accepted signing modes, for callers that
// need to enumerate them (the CLI validator and the guide --json catalogue). A
// copy, so a caller holding the result cannot mutate the canonical list.
func SignModes() []string {
	out := make([]string, len(validSignModes))
	copy(out, validSignModes)
	return out
}

// IsValidSignMode reports whether mode is a recognised signing mode. It reads the
// unexported canonical list, so its verdict cannot be altered by another package.
func IsValidSignMode(mode string) bool {
	for _, m := range validSignModes {
		if mode == m {
			return true
		}
	}
	return false
}

// ApplySpec is the change to run across a selection via multi-gitter. It is
// assembled in cmd/ from flags.
type ApplySpec struct {
	Branch        string
	BaseBranch    string // base for the PR (e.g. "main" or "dev"); empty = repo default
	CommitMessage string
	PRTitle       string
	PRBody        string
	Labels        []string
	Reviewers     []string
	Draft         bool
	DryRun        bool

	// Confirm authorizes a live (non-dry-run) apply that opens PRs. apply.Apply
	// refuses a run with DryRun=false && Confirm=false, so the charter invariant
	// "a real run needs an explicit confirmation" holds at the execution boundary
	// even for a caller that bypasses the Cobra --confirm flag (e.g. a future MCP
	// adapter), not just in cmd/.
	Confirm bool

	Script []string // the command to run in each repo, e.g. ["sed", "-i", ...]

	// Sign selects how commits are signed: SignLocal (the operator's own GPG key
	// via the git binary), SignGitHub (GitHub's web-flow key via the API), or
	// SignNone (unsigned). There is no default — a real run must set it.
	Sign string

	// BatchSize and BatchPause throttle PR creation to stay under GitHub's
	// secondary rate limits (80 content-generating requests/min). When BatchSize
	// > 0, apply runs multi-gitter over the selection in chunks of that many
	// repos, sleeping BatchPause between chunks. Zero BatchSize = one run over the
	// whole selection (no throttling). Note: neither beats GitHub's 500
	// content-request/hour ceiling — a large fleet must spread across hours, which
	// re-running (multi-gitter skips repos already done) does naturally.
	BatchSize  int
	BatchPause time.Duration
}

// Selection is the frozen set of repos a run targets: the shared artifact that
// both `mirror` (ghorg) and `apply` (multi-gitter) consume, so they operate on a
// provably identical set.
type Selection struct {
	Version    int             `json:"version"`
	Owner      string          `json:"owner"`
	OwnerType  string          `json:"ownerType"` // "User" | "Organization"
	Filter     SelectionFilter `json:"filter"`
	ResolvedAt time.Time       `json:"resolvedAt"`
	Tool       string          `json:"tool"`
	Repos      []Repo          `json:"repos"`

	// BranchesChecked lists the branch names whose presence was recorded at
	// selection time (via `select --branch-presence`). A repo's BranchPresence
	// map holds one entry per name here; a name equal to the repo's own default
	// branch is recorded as present without an API call.
	BranchesChecked []string `json:"branchesChecked,omitempty"`
}
