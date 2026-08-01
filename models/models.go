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

// SelectionFilter records how a selection was resolved, for provenance.
type SelectionFilter struct {
	AllRepos bool     `json:"allRepos"`
	Topics   []string `json:"topics,omitempty"`
}

// SelectionVersion is the current lockfile schema version.
const SelectionVersion = 1

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
