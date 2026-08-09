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
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "guide",
		Short: "Print an operator playbook for humans and AI agents driving goldfinger",
		RunE: func(cmd *cobra.Command, args []string) error {
			if asJSON {
				// The machine-consumable CLI catalogue is derived from the live
				// command tree (cmd.Root()), so it can never omit a registered command.
				return emitJSON(cmd.OutOrStdout(), buildCapabilities(cmd.Root()), quietRequested(cmd))
			}
			fmt.Fprint(cmd.OutOrStdout(), guideText)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false,
		"emit a versioned machine-readable capabilities catalogue (commands, flags, required flags, enum values, and a canonical example per command) to stdout instead of the prose playbook")
	return cmd
}
