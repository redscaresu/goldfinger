package main

import "github.com/spf13/cobra"

// version is the binary version. It is overridden at release time via
// -ldflags "-X main.version=<tag>".
var version = "dev"

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:     "goldfinger",
		Short:   "Fan out changes across GitHub repos at scale",
		Version: version,
		// Validation errors are actionable on their own; don't bury them
		// under a full usage dump.
		SilenceUsage: true,
	}
	root.AddCommand(newReposCmd(), newRunCmd())
	return root
}
