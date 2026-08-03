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
}

// FullName returns the canonical "owner/name" identifier.
func (r Repo) FullName() string {
	return r.Owner + "/" + r.Name
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
}

// SelectionVersion is the current lockfile schema version.
const SelectionVersion = 1

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
	Script        []string // the command to run in each repo, e.g. ["sed", "-i", ...]
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
}
