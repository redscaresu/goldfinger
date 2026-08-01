package main

import (
	"errors"
	"os"

	"github.com/redscaresu/goldfinger/apply"
	"github.com/redscaresu/goldfinger/models"
	"github.com/redscaresu/goldfinger/selection"
	"github.com/spf13/cobra"
)

func newApplyCmd() *cobra.Command {
	var (
		selectionPath string
		branch        string
		baseBranch    string
		commitMessage string
		prTitle       string
		prBody        string
		labels        []string
		reviewers     []string
		draft         bool
		dryRun        bool
		confirm       bool
	)
	cmd := &cobra.Command{
		Use:   "apply [flags] -- command [args...]",
		Short: "Run a change across the selection and open PRs via multi-gitter",
		RunE: func(cmd *cobra.Command, args []string) error {
			token := os.Getenv(tokenEnvVar)
			if err := validateToken(token); err != nil {
				return err
			}
			script := scriptArgs(cmd, args)
			if err := validateApply(applyValidation{
				branch:        branch,
				commitMessage: commitMessage,
				prTitle:       prTitle,
				script:        script,
			}); err != nil {
				return err
			}
			// Safety guard: a real run opens PRs. Require an explicit --confirm
			// so it can never happen by omitting a flag.
			if !dryRun && !confirm {
				return errors.New("refusing to open PRs: re-run with --confirm to disable the dry-run safety, or keep --dry-run")
			}
			if err := requireTool("multi-gitter", "https://github.com/lindell/multi-gitter#installation"); err != nil {
				return err
			}
			sel, err := selection.Read(selectionPath)
			if err != nil {
				return err
			}
			spec := models.ApplySpec{
				Branch:        branch,
				BaseBranch:    baseBranch,
				CommitMessage: commitMessage,
				PRTitle:       prTitle,
				PRBody:        prBody,
				Labels:        labels,
				Reviewers:     reviewers,
				Draft:         draft,
				DryRun:        dryRun,
				Script:        script,
			}
			return apply.Apply(cmd.Context(), execRun, sel, spec, token)
		},
	}
	f := cmd.Flags()
	f.StringVar(&selectionPath, "selection", defaultSelectionPath, "path to the selection lockfile")
	f.StringVar(&branch, "branch", "", "branch to commit changes to (required)")
	f.StringVar(&baseBranch, "base-branch", "", "base branch for the PR, e.g. main or dev (default: repo default branch)")
	f.StringVar(&commitMessage, "commit-message", "", "commit message (required)")
	f.StringVar(&prTitle, "pr-title", "", "pull request title (required)")
	f.StringVar(&prBody, "pr-body", "", "pull request body")
	f.StringArrayVar(&labels, "label", nil, "label to add to every PR (repeatable)")
	f.StringArrayVar(&reviewers, "reviewer", nil, "reviewer to request: user or org/team (repeatable)")
	f.BoolVar(&draft, "draft", false, "open PRs as drafts")
	f.BoolVar(&dryRun, "dry-run", true, "run without pushing or opening PRs (default; pass --dry-run=false for a real run)")
	f.BoolVar(&confirm, "confirm", false, "required alongside --dry-run=false to actually open PRs")
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
