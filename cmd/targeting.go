package main

import "github.com/spf13/cobra"

// targeting holds the repo-selection flags for `select`. It names the target
// owner and exactly one of three mutually exclusive modes: every repo
// (allRepos), a topic filter (topics), or an EXPLICIT set of named repos (repos
// / reposFrom).
type targeting struct {
	org      string
	allRepos bool
	topics   []string

	// repos and reposFrom drive an explicit selection: repos are basenames passed
	// via --repo (repeatable); reposFrom is a file of basenames (--repos-from).
	// Owner always comes from --org (single-owner model), so entries are bare
	// names. Both feed resolveTargetRepos, which merges, normalises, and dedupes
	// them into the final list.
	repos     []string
	reposFrom string
}

// explicit reports whether targeting requests an explicit selection — the
// operator named repos via --repo or --repos-from rather than a filter. reposFrom
// counts even after its file has been read into repos, so an empty --repos-from
// file (a zero-repo explicit set, valid under --allow-empty) is still recognised
// as explicit mode and never mistaken for a filter selection.
func (t targeting) explicit() bool {
	return len(t.repos) > 0 || t.reposFrom != ""
}

// addTargetingFlags binds the shared targeting flags onto cmd.
func addTargetingFlags(cmd *cobra.Command, t *targeting) {
	f := cmd.Flags()
	f.StringVar(&t.org, "org", "", "GitHub org to target (required)")
	f.BoolVar(&t.allRepos, "all-repos", false, "target every non-archived repo in the org")
	f.StringArrayVar(&t.topics, "topic", nil, "target repos carrying this topic (repeatable)")
	f.StringArrayVar(&t.repos, "repo", nil,
		"target this repo by bare name under --org (repeatable); an EXPLICIT selection, mutually exclusive with --all-repos/--topic. A name that 404s is a hard error")
	f.StringVar(&t.reposFrom, "repos-from", "",
		"target the repos named in this file: one bare name per line under --org, blank lines and #-comments ignored (an explicit selection)")
}
