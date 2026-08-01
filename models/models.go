// Package models holds the domain types shared across goldfinger's packages.
// It has no dependencies on other goldfinger packages.
package models

// Repo is a GitHub repository goldfinger can operate on.
type Repo struct {
	Owner         string
	Name          string
	CloneURL      string // https
	DefaultBranch string
	Topics        []string
	Archived      bool
}

// FullName returns the canonical "owner/name" identifier.
func (r Repo) FullName() string {
	return r.Owner + "/" + r.Name
}

// Status is the outcome of processing a single repo during a run.
type Status string

const (
	StatusSuccess Status = "success"
	StatusSkipped Status = "skipped"
	StatusFailed  Status = "failed"
)

// RepoResult is the per-repo outcome of a run.
type RepoResult struct {
	Repo   Repo
	Status Status
	PRURL  string // set when Status is StatusSuccess
	Err    error  // set when Status is StatusFailed
}

// RunSpec carries everything the run engine needs. It is assembled in cmd/
// from validated flags.
type RunSpec struct {
	Branch        string
	CommitMessage string
	PRTitle       string
	PRBody        string
	Labels        []string
	Reviewers     []string // users or org/team slugs
	Draft         bool
	Script        []string
	Concurrency   int
	DryRun        bool
}
