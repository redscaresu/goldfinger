package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/redscaresu/goldfinger/client"
	"github.com/redscaresu/goldfinger/discovery"
	"github.com/redscaresu/goldfinger/models"
	"github.com/redscaresu/goldfinger/selection"
	"github.com/spf13/cobra"
)

// defaultSelectionPath is where the lockfile lives unless --selection overrides.
const defaultSelectionPath = "goldfinger.selection"

// repoResolver is the slice of the GitHub client that `select` needs. Defining
// it here (consumer side) lets the command's logic be tested with a fake, no
// network required. It is satisfied by *client.Client.
type repoResolver interface {
	Verify(ctx context.Context) (string, error)
	ListRepos(ctx context.Context, owner string) ([]models.Repo, string, error)
}

func newSelectCmd() *cobra.Command {
	var (
		t             targeting
		selectionPath string
	)
	cmd := &cobra.Command{
		Use:   "select",
		Short: "Resolve an owner's repos by topic and freeze them as a selection",
		RunE: func(cmd *cobra.Command, args []string) error {
			token := os.Getenv(tokenEnvVar)
			if err := validateToken(token); err != nil {
				return err
			}
			if err := validateTargeting(t); err != nil {
				return err
			}
			c, err := client.New(token)
			if err != nil {
				return err
			}
			return runSelect(cmd.Context(), c, t, selectionPath, "goldfinger "+version, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	addTargetingFlags(cmd, &t)
	cmd.Flags().StringVar(&selectionPath, "selection", defaultSelectionPath, "path to write the selection lockfile")
	return cmd
}

// runSelect resolves the target repos, filters them, and writes the selection
// lockfile. It is the testable core of the select command.
func runSelect(ctx context.Context, r repoResolver, t targeting, selectionPath, tool string, out, errOut io.Writer) error {
	if _, err := r.Verify(ctx); err != nil {
		return fmt.Errorf("verifying token: %w", err)
	}
	repos, ownerType, err := r.ListRepos(ctx, t.org)
	if err != nil {
		return err
	}
	selected := discovery.Select(repos, discovery.Filter{AllRepos: t.allRepos, Topics: t.topics})

	sel := models.Selection{
		Version:    models.SelectionVersion,
		Owner:      t.org,
		OwnerType:  ownerType,
		Filter:     models.SelectionFilter{AllRepos: t.allRepos, Topics: t.topics},
		ResolvedAt: time.Now().UTC(),
		Tool:       tool,
		Repos:      selected,
	}
	if err := selection.Write(selectionPath, sel); err != nil {
		return err
	}

	for _, repo := range selected {
		fmt.Fprintln(out, repo.FullName())
	}
	fmt.Fprintf(errOut, "\n%d repo(s) written to %s\n", len(selected), selectionPath)
	return nil
}
