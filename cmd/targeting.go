package main

import "github.com/spf13/cobra"

// targeting holds the repo-selection flags shared by the `repos` and `run`
// commands.
type targeting struct {
	org      string
	allRepos bool
	topics   []string
}

// addTargetingFlags binds the shared targeting flags onto cmd.
func addTargetingFlags(cmd *cobra.Command, t *targeting) {
	f := cmd.Flags()
	f.StringVar(&t.org, "org", "", "GitHub org to target (required)")
	f.BoolVar(&t.allRepos, "all-repos", false, "target every non-archived repo in the org")
	f.StringArrayVar(&t.topics, "topic", nil, "target repos carrying this topic (repeatable)")
}
