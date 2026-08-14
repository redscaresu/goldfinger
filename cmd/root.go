package main

import (
	"github.com/redscaresu/goldfinger/models"
	"github.com/spf13/cobra"
)

// version is the binary version. It is overridden at release time via
// -ldflags "-X main.version=<tag>".
var version = "dev"

// tokenEnvVar is the environment variable holding the GitHub PAT.
const tokenEnvVar = models.TokenEnvVar

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:     "goldfinger",
		Short:   "Fan out changes across GitHub repos at scale",
		Version: version,
		// Validation errors are actionable on their own; don't bury them
		// under a full usage dump.
		SilenceUsage: true,
		// main owns error printing and the process exit code, so that `check`
		// can exit non-zero for drift without Cobra printing an "Error:" line.
		SilenceErrors: true,
	}
	root.PersistentFlags().BoolP(quietFlagName, "q", false,
		"silence human progress/decorations on stderr and keep stdout to the command's machine result; JSON payloads are emitted compact (single-line) to cost an agent fewer tokens")
	root.AddCommand(newSelectCmd(), newMirrorCmd(), newApplyCmd(), newCheckCmd(), newScanCmd(), newSelectionsCmd(), newDoctorCmd(), newGuideCmd(), newSchemaCmd(), newWorkspacesCmd(), newMCPCmd())
	return root
}
