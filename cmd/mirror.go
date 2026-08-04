package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/redscaresu/goldfinger/mirror"
	"github.com/redscaresu/goldfinger/models"
	"github.com/redscaresu/goldfinger/selection"
	"github.com/spf13/cobra"
)

func newMirrorCmd() *cobra.Command {
	var (
		selectionPath string
		name          string
		workspace     string
		purpose       string
		branch        string
		concurrency   int
		cloneDepth    int
		noClean       bool
		dryRun        bool
	)
	cmd := &cobra.Command{
		Use:   "mirror",
		Short: "Clone the selection into a local workspace via ghorg",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Pure local flag check first, before any token/tool/selection work,
			// so an impossible flag combo fails fast without needing a token.
			if err := validateMirror(mirrorValidation{branch: branch, cloneDepth: cloneDepth}); err != nil {
				return err
			}
			token, source, err := resolveToken(cmd.Context())
			if err != nil {
				return err
			}
			announceTokenSource(cmd.ErrOrStderr(), source)
			if err := requireTool("ghorg", "https://github.com/gabrie30/ghorg#installation"); err != nil {
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
			ws, err := resolveWorkspace(workspace, purpose, branch)
			if err != nil {
				return err
			}
			return runMirror(cmd.Context(), execRun, sel, ws, token, mirror.Options{
				Branch:      branch,
				Concurrency: concurrency,
				CloneDepth:  cloneDepth,
				NoClean:     noClean,
				DryRun:      dryRun,
			}, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	addSelectionFlags(cmd, &name, &selectionPath)
	f := cmd.Flags()
	f.StringVar(&workspace, "workspace", "", "absolute workspace dir (default ~/goldfinger; repos land in <workspace>/<owner>)")
	f.StringVar(&purpose, "purpose", "", "ephemeral, timestamped workspace ~/goldfinger/<purpose>-<YYYY-MM-DD-HHMMSS.mmm> (goldfinger stamps the time to the millisecond; you clean the dir up when done); mutually exclusive with --workspace")
	f.StringVar(&branch, "branch", "", "checkout this branch in every cloned repo (one name for all repos; ghorg leaves a repo on its default branch where the branch is absent). With --purpose it is also folded into the dir name: <purpose>-<branch>-<stamp>. Default: each repo's own default branch")
	f.IntVar(&concurrency, "concurrency", 0, "concurrent clones (0 = ghorg default)")
	f.IntVar(&cloneDepth, "clone-depth", 0, "shallow clone depth (0 = full history). Incompatible with --branch: a shallow clone only fetches each repo's default branch, so --branch would silently fall back to the default")
	f.BoolVar(&noClean, "no-clean", false, "preserve local changes in existing clones (skip ghorg's git clean on re-sync)")
	f.BoolVar(&dryRun, "dry-run", false, "show what ghorg would clone without cloning")
	return cmd
}

// runMirror frames the mirror phase and delegates to the mirror package. It is
// the testable core of the mirror command (the Runner seam lets tests exercise
// it without ghorg installed).
//
// The resolved workspace path is the one machine-readable line goldfinger emits:
// it goes to out (stdout) as a bare absolute path so a script can capture it
// (ws=$(goldfinger mirror ... 2>log)), while every human banner and the ghorg
// child's own output stay on errOut (stderr) to keep stdout parseable. The path
// prints before delegating: if ghorg later fails, stdout still holds the
// intended path, so callers must check the exit code, not stdout, for success.
func runMirror(ctx context.Context, run mirror.Runner, sel models.Selection, ws, token string, opts mirror.Options, out, errOut io.Writer) error {
	opts.Workspace = ws
	fmt.Fprintln(out, ws)
	banner(errOut, fmt.Sprintf("Mirroring %d repo(s) into %s", len(sel.Repos), ws))
	if err := mirror.Mirror(ctx, run, sel, token, opts); err != nil {
		return err
	}
	done(errOut, fmt.Sprintf("mirror complete → %s/%s", ws, sel.Owner))
	return nil
}

// nowFunc is the clock used to timestamp ephemeral --purpose workspaces. It is
// a package var so tests can pin the time.
var nowFunc = time.Now

// resolveWorkspace returns an absolute workspace directory. ghorg requires an
// absolute --path. There are three cases, in priority order:
//   - --purpose: an ephemeral, timestamped dir
//     ~/goldfinger/<purpose>[-<branch>]-<YYYY-MM-DD-HHMMSS.mmm>. goldfinger
//     stamps the time to the millisecond so the operator supplies only the
//     purpose (and, when mirroring a specific --branch, that branch is folded
//     into the name too) and each run gets its own pristine dir; goldfinger
//     never deletes it — the operator cleans it up. Mutually exclusive with
//     --workspace.
//   - --workspace: used as given (made absolute).
//   - neither: defaults to ~/goldfinger.
func resolveWorkspace(workspace, purpose, branch string) (string, error) {
	if workspace != "" && purpose != "" {
		return "", errors.New("--workspace and --purpose are mutually exclusive")
	}
	if purpose != "" {
		if err := validatePurpose(purpose); err != nil {
			return "", err
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir for workspace: %w", err)
		}
		dir := purpose
		if branch != "" {
			// The real branch (with any slashes) still goes to ghorg; only the
			// dir-name component is sanitised so it stays a single safe segment.
			dir += "-" + sanitizeForDir(branch)
		}
		dir += "-" + nowFunc().Format("2006-01-02-150405.000")
		return filepath.Join(home, "goldfinger", dir), nil
	}
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

// validatePurpose rejects anything that isn't a plain directory-name component,
// so --purpose can't traverse out of ~/goldfinger or produce a surprising path.
func validatePurpose(p string) error {
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("--purpose %q: only letters, digits, and - _ . are allowed (it becomes a directory name)", p)
		}
	}
	if strings.Contains(p, "..") {
		return fmt.Errorf("--purpose %q must not contain %q", p, "..")
	}
	return nil
}

// sanitizeForDir maps any character that isn't a plain directory-name component
// (letters, digits, - _ .) to '-', so a branch like "feature/x" folds into a
// single safe path segment ("feature-x") in a --purpose workspace name. The
// original branch is still passed to ghorg unchanged.
func sanitizeForDir(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}
