package main

import (
	"fmt"
	"os"

	"github.com/redscaresu/goldfinger/models"
	"github.com/spf13/cobra"
)

// runOpts holds every flag the run command binds. Grouping them in a struct
// keeps the flags that later steps consume from tripping unused-variable
// checks while the engine is still being wired.
type runOpts struct {
	targeting

	branch        string
	commitMessage string
	prTitle       string
	prBody        string
	labels        []string
	reviewers     []string
	draft         bool
	concurrency   int
	sparse        []string
	maxRepos      int
	dryRun        bool
	output        string
}

func newRunCmd() *cobra.Command {
	var o runOpts
	cmd := &cobra.Command{
		Use:   "run [flags] -- command [args...]",
		Short: "Clone matching repos, run a script in each, and open PRs",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateToken(os.Getenv("GITHUB_TOKEN")); err != nil {
				return err
			}
			if err := validateTargeting(o.targeting); err != nil {
				return err
			}
			script := scriptArgs(cmd, args)
			if err := validateRun(runValidation{
				branch:        o.branch,
				commitMessage: o.commitMessage,
				prTitle:       o.prTitle,
				script:        script,
			}); err != nil {
				return err
			}
			if err := validateOutput(o.output); err != nil {
				return err
			}
			spec := models.RunSpec{
				Branch:        o.branch,
				CommitMessage: o.commitMessage,
				PRTitle:       o.prTitle,
				PRBody:        o.prBody,
				Labels:        o.labels,
				Reviewers:     o.reviewers,
				Draft:         o.draft,
				Script:        script,
				Concurrency:   o.concurrency,
				DryRun:        o.dryRun,
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"run: would process a %d-part script on branch %q (engine not yet wired, build-order step 4)\n",
				len(spec.Script), spec.Branch)
			return nil
		},
	}
	addTargetingFlags(cmd, &o.targeting)
	f := cmd.Flags()
	f.StringVar(&o.branch, "branch", "", "branch name to create for the change (required)")
	f.StringVar(&o.commitMessage, "commit-message", "", "commit message (required)")
	f.StringVar(&o.prTitle, "pr-title", "", "pull request title (required)")
	f.StringVar(&o.prBody, "pr-body", "", "pull request body")
	f.StringArrayVar(&o.labels, "label", nil, "label to apply to every PR (repeatable)")
	f.StringArrayVar(&o.reviewers, "reviewer", nil, "reviewer to request: user or org/team (repeatable)")
	f.BoolVar(&o.draft, "draft", false, "open PRs as drafts")
	f.IntVar(&o.concurrency, "concurrency", 10, "repos cloned and scripted in parallel")
	f.StringArrayVar(&o.sparse, "sparse", nil, "sparse-checkout path: only fetch these paths (repeatable)")
	f.IntVar(&o.maxRepos, "max-repos", 50, "refuse to operate on more than this many repos")
	f.BoolVar(&o.dryRun, "dry-run", false, "clone and run the script but do not push or open PRs")
	f.StringVar(&o.output, "output", "table", "output format: table or json")
	return cmd
}

// scriptArgs returns the command supplied after the `--` separator, or nil if
// no `--` was present.
func scriptArgs(cmd *cobra.Command, args []string) []string {
	dash := cmd.ArgsLenAtDash()
	if dash < 0 {
		return nil
	}
	return args[dash:]
}
