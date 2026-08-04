package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/redscaresu/goldfinger/client"
	"github.com/redscaresu/goldfinger/discovery"
	"github.com/redscaresu/goldfinger/models"
	"github.com/redscaresu/goldfinger/selection"
	"github.com/spf13/cobra"
)

// defaultSelectionPath is where the lockfile lives unless --selection overrides.
const defaultSelectionPath = "goldfinger.selection"

// repoResolver is the slice of the GitHub client that `select` and `check` need.
// Defining it here (consumer side) lets the commands' logic be tested with a
// fake, no network required. It is satisfied by *client.Client.
type repoResolver interface {
	Verify(ctx context.Context) (string, error)
	ListRepos(ctx context.Context, owner string) ([]models.Repo, string, error)
}

// branchResolver extends repoResolver with the read-only branch lookup that
// `select --branch-presence` needs. `check` keeps to the narrower repoResolver,
// so it does not depend on a method it never calls.
type branchResolver interface {
	repoResolver
	BranchExists(ctx context.Context, owner, repo, branch string) (bool, error)
}

func newSelectCmd() *cobra.Command {
	var (
		t               targeting
		selectionPath   string
		name            string
		branchesToCheck []string
	)
	cmd := &cobra.Command{
		Use:   "select",
		Short: "Resolve an owner's repos by topic and freeze them as a selection",
		RunE: func(cmd *cobra.Command, args []string) error {
			token, source, err := resolveToken(cmd.Context())
			if err != nil {
				return err
			}
			announceTokenSource(cmd.ErrOrStderr(), source)
			if err := validateTargeting(t); err != nil {
				return err
			}
			path, err := resolveSelectionPath(name, selectionPath)
			if err != nil {
				return err
			}
			c, err := client.New(token)
			if err != nil {
				return err
			}
			return runSelect(cmd.Context(), c, t, branchesToCheck, path, "goldfinger "+version, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	addTargetingFlags(cmd, &t)
	addSelectionFlags(cmd, &name, &selectionPath)
	cmd.Flags().StringArrayVar(&branchesToCheck, "branch-presence", nil,
		"record (read-only) whether this branch exists on each selected repo, frozen into the lockfile for a later `mirror --branch` (repeatable). Facts are recorded at selection time and can drift")
	return cmd
}

// runSelect resolves the target repos, filters them, annotates the selected set
// with any requested branch-presence facts, and writes the selection lockfile.
// It is the testable core of the select command.
func runSelect(ctx context.Context, r branchResolver, t targeting, branchesToCheck []string, selectionPath, tool string, out, errOut io.Writer) error {
	banner(errOut, "Resolving selection for "+t.org)
	if _, err := r.Verify(ctx); err != nil {
		return fmt.Errorf("verifying token: %w", err)
	}
	repos, ownerType, err := r.ListRepos(ctx, t.org)
	if err != nil {
		return err
	}
	selected := discovery.Select(repos, discovery.Filter{AllRepos: t.allRepos, Topics: t.topics})

	branches := dedupeNonEmpty(branchesToCheck)
	if err := annotateBranchPresence(ctx, r, selected, branches, errOut); err != nil {
		return err
	}

	sel := models.Selection{
		Version:         models.SelectionVersion,
		Owner:           t.org,
		OwnerType:       ownerType,
		Filter:          models.SelectionFilter{AllRepos: t.allRepos, Topics: t.topics},
		ResolvedAt:      time.Now().UTC(),
		Tool:            tool,
		Repos:           selected,
		BranchesChecked: branches,
	}
	if err := selection.Write(selectionPath, sel); err != nil {
		return err
	}

	for _, repo := range selected {
		fmt.Fprintln(out, repo.FullName())
	}
	done(errOut, fmt.Sprintf("%d repo(s) written to %s", len(selected), selectionPath))
	return nil
}

// annotateBranchPresence records, for each selected repo, whether each requested
// branch exists — via read-only REST. A branch equal to the repo's own default
// is present by definition and short-circuits without an API call. selected is
// mutated in place. discovery.Select has already dropped archived repos, so only
// the live selection is probed.
func annotateBranchPresence(ctx context.Context, r branchResolver, selected []models.Repo, branches []string, errOut io.Writer) error {
	if len(branches) == 0 {
		return nil
	}
	banner(errOut, fmt.Sprintf("Recording branch presence (%d branch(es)) across %d repo(s)", len(branches), len(selected)))
	for i := range selected {
		repo := &selected[i]
		for _, b := range branches {
			if b == repo.DefaultBranch {
				recordPresence(repo, b, true)
				continue
			}
			has, err := r.BranchExists(ctx, repo.Owner, repo.Name, b)
			if err != nil {
				return err
			}
			recordPresence(repo, b, has)
		}
	}
	return nil
}

// recordPresence stores present for branch on repo, allocating the map lazily.
func recordPresence(repo *models.Repo, branch string, present bool) {
	if repo.BranchPresence == nil {
		repo.BranchPresence = make(map[string]bool)
	}
	repo.BranchPresence[branch] = present
}

// dedupeNonEmpty returns in with empty and duplicate entries removed, preserving
// first-seen order — so `--branch-presence dev --branch-presence dev` probes dev
// once and BranchesChecked lists it once.
func dedupeNonEmpty(in []string) []string {
	seen := make(map[string]bool, len(in))
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
