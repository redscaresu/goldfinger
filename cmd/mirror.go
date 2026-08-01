package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/redscaresu/goldfinger/mirror"
	"github.com/redscaresu/goldfinger/selection"
	"github.com/spf13/cobra"
)

func newMirrorCmd() *cobra.Command {
	var (
		selectionPath string
		workspace     string
		concurrency   int
		cloneDepth    int
		dryRun        bool
	)
	cmd := &cobra.Command{
		Use:   "mirror",
		Short: "Clone the selection into a local workspace via ghorg",
		RunE: func(cmd *cobra.Command, args []string) error {
			token := os.Getenv(tokenEnvVar)
			if err := validateToken(token); err != nil {
				return err
			}
			if err := requireTool("ghorg", "https://github.com/gabrie30/ghorg#installation"); err != nil {
				return err
			}
			sel, err := selection.Read(selectionPath)
			if err != nil {
				return err
			}
			ws, err := resolveWorkspace(workspace)
			if err != nil {
				return err
			}
			return mirror.Mirror(cmd.Context(), execRun, sel, token, mirror.Options{
				Workspace:   ws,
				Concurrency: concurrency,
				CloneDepth:  cloneDepth,
				DryRun:      dryRun,
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&selectionPath, "selection", defaultSelectionPath, "path to the selection lockfile")
	f.StringVar(&workspace, "workspace", "", "absolute workspace dir (default ~/goldfinger; repos land in <workspace>/<owner>)")
	f.IntVar(&concurrency, "concurrency", 0, "concurrent clones (0 = ghorg default)")
	f.IntVar(&cloneDepth, "clone-depth", 0, "shallow clone depth (0 = full history)")
	f.BoolVar(&dryRun, "dry-run", false, "show what ghorg would clone without cloning")
	return cmd
}

// resolveWorkspace returns an absolute workspace directory, defaulting to
// ~/goldfinger. ghorg requires an absolute --path.
func resolveWorkspace(workspace string) (string, error) {
	if workspace == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir for workspace: %w", err)
		}
		return filepath.Join(home, "goldfinger"), nil
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("resolve workspace path: %w", err)
	}
	return abs, nil
}
