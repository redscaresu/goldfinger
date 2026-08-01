package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func newReposCmd() *cobra.Command {
	var t targeting
	cmd := &cobra.Command{
		Use:   "repos",
		Short: "List repos in an org matching the targeting filters",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateToken(os.Getenv("GITHUB_TOKEN")); err != nil {
				return err
			}
			if err := validateTargeting(t); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "repos: discovery not yet wired (build-order step 2)")
			return nil
		},
	}
	addTargetingFlags(cmd, &t)
	return cmd
}
