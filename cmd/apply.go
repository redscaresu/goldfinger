package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/redscaresu/goldfinger/apply"
	"github.com/redscaresu/goldfinger/models"
	"github.com/redscaresu/goldfinger/selection"
	"github.com/spf13/cobra"
)

func newApplyCmd() *cobra.Command {
	var (
		selectionPath string
		name          string
		branch        string
		baseBranch    string
		commitMessage string
		prTitle       string
		prBody        string
		prBodyFile    string
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
			token, err := resolveToken()
			if err != nil {
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
			// A long PR body is easier to supply from a file than a shell-quoted
			// flag; --pr-body-file loads it. The two body sources are mutually
			// exclusive so there's no ambiguity about which one wins.
			body, err := resolvePRBody(prBody, prBodyFile)
			if err != nil {
				return err
			}
			prBody = body
			// Safety guard: a real run opens PRs. Require an explicit --confirm
			// so it can never happen by omitting a flag.
			if !dryRun && !confirm {
				return errors.New("refusing to open PRs: re-run with --confirm to disable the dry-run safety, or keep --dry-run")
			}
			if err := requireTool("multi-gitter", "https://github.com/lindell/multi-gitter#installation"); err != nil {
				return err
			}
			path, err := resolveSelectionPath(name, selectionPath)
			if err != nil {
				return err
			}
			sel, err := selection.Read(path)
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
			return runApply(cmd.Context(), execRun, sel, spec, token, cmd.ErrOrStderr())
		},
	}
	addSelectionFlags(cmd, &name, &selectionPath)
	f := cmd.Flags()
	f.StringVar(&branch, "branch", "", "branch to commit changes to (required)")
	f.StringVar(&baseBranch, "base-branch", "", "base branch for the PR, e.g. main or dev (default: repo default branch)")
	f.StringVar(&commitMessage, "commit-message", "", "commit message (required)")
	f.StringVar(&prTitle, "pr-title", "", "pull request title (required)")
	f.StringVar(&prBody, "pr-body", "", "pull request body (mutually exclusive with --pr-body-file)")
	f.StringVar(&prBodyFile, "pr-body-file", "", "read the pull request body from a file (mutually exclusive with --pr-body)")
	f.StringArrayVar(&labels, "label", nil, "label to add to every PR (repeatable)")
	f.StringArrayVar(&reviewers, "reviewer", nil, "reviewer to request: user or org/team (repeatable)")
	f.BoolVar(&draft, "draft", false, "open PRs as drafts")
	f.BoolVar(&dryRun, "dry-run", true, "run without pushing or opening PRs (default; pass --dry-run=false for a real run)")
	f.BoolVar(&confirm, "confirm", false, "required alongside --dry-run=false to actually open PRs")
	return cmd
}

// runApply frames the apply phase and delegates to the apply package. It is the
// testable core of the apply command.
func runApply(ctx context.Context, run apply.Runner, sel models.Selection, spec models.ApplySpec, token string, errOut io.Writer) error {
	mode := "LIVE — opening PRs"
	if spec.DryRun {
		mode = "dry-run — no push, no PRs"
	}
	baseLabel := spec.BaseBranch
	if baseLabel == "" {
		baseLabel = "each repo's default branch"
	}
	banner(errOut, fmt.Sprintf("Applying to %d repo(s) [%s] onto base %s", len(sel.Repos), mode, baseLabel))
	// Spell out the base per repo so the routing is auditable before anything
	// runs — this is exactly what a mixed dev/main selection needs to confirm
	// each PR lands on the right branch.
	for _, r := range sel.Repos {
		fmt.Fprintf(errOut, "  %s -> %s\n", r.FullName(), resolveBase(spec.BaseBranch, r))
	}
	// Without a global --base-branch, goldfinger passes no base to multi-gitter,
	// which targets each repo's *live* default at run time. The branches printed
	// above are the defaults recorded at selection time, so flag that they can
	// drift rather than presenting them as the guaranteed target.
	if spec.BaseBranch == "" {
		fmt.Fprintln(errOut, "  (branches shown are each repo's default recorded at selection; multi-gitter targets the live default at run time)")
	}
	if err := apply.Apply(ctx, run, sel, spec, token); err != nil {
		return err
	}
	done(errOut, "apply complete")
	return nil
}

// resolvePRBody picks the PR body from either the inline --pr-body or the
// --pr-body-file path. Supplying both is an error; supplying neither yields an
// empty body.
func resolvePRBody(inline, path string) (string, error) {
	if path == "" {
		return inline, nil
	}
	if inline != "" {
		return "", errors.New("--pr-body and --pr-body-file are mutually exclusive")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading --pr-body-file: %w", err)
	}
	return string(b), nil
}

// resolveBase reports the branch a repo's PR will target: an explicit global
// --base-branch wins; otherwise multi-gitter uses the repo's own default
// branch. Falls back to a readable label when the lockfile lacks the default
// (e.g. an older selection or a hand-written one).
func resolveBase(globalBase string, r models.Repo) string {
	if globalBase != "" {
		return globalBase
	}
	if r.DefaultBranch != "" {
		return r.DefaultBranch
	}
	return "repo default"
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
