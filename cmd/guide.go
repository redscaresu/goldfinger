package main

import (
	_ "embed"
	"fmt"

	"github.com/spf13/cobra"
)

// guideText is the operator playbook, embedded so it ships with the binary and
// is reachable at runtime by any agent driving goldfinger, wherever it runs.
//
//go:embed guide.md
var guideText string

func newGuideCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "guide",
		Short: "Print an operator playbook for humans and AI agents driving goldfinger",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprint(cmd.OutOrStdout(), guideText)
			return nil
		},
	}
}
