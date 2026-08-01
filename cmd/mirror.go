package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/redscaresu/goldfinger/mirror"
	"github.com/redscaresu/goldfinger/models"
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
			return runMirror(cmd.Context(), execRun, sel, ws, token, mirror.Options{
				Concurrency: concurrency,
				CloneDepth:  cloneDepth,
				DryRun:      dryRun,
			}, cmd.ErrOrStderr())
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

// runMirror frames the mirror phase and delegates to the mirror package. It is
// the testable core of the mirror command (the Runner seam lets tests exercise
// it without ghorg installed).
func runMirror(ctx context.Context, run mirror.Runner, sel models.Selection, ws, token string, opts mirror.Options, errOut io.Writer) error {
	opts.Workspace = ws
	banner(errOut, fmt.Sprintf("Mirroring %d repo(s) into %s", len(sel.Repos), ws))
	if err := mirror.Mirror(ctx, run, sel, token, opts); err != nil {
		return err
	}
	done(errOut, fmt.Sprintf("mirror complete → %s/%s", ws, sel.Owner))
	return nil
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
