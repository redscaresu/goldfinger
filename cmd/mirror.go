package main

import (
	"context"
	"encoding/json"
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

// mirrorReportName is the filename `--write-report` writes under the workspace.
const mirrorReportName = "goldfinger-mirror.json"

// reportOptions selects which machine-readable report outputs a mirror emits.
type reportOptions struct {
	toStdout bool // --report-json: print the report JSON to stdout
	toFile   bool // --write-report: write <workspace>/goldfinger-mirror.json (only on success)
}

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
		reportJSON    bool
		writeReport   bool
	)
	cmd := &cobra.Command{
		Use:   "mirror",
		Short: "Clone the selection into a local workspace via ghorg",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Pure/local validation first — flag combos, selection path, workspace —
			// so a bad invocation fails without resolving a token (which can shell out
			// to `gh`) or hitting the network.
			if err := validateMirror(mirrorValidation{branch: branch, cloneDepth: cloneDepth}); err != nil {
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
			ws, snap, err := resolveWorkspace(workspace, purpose, branch)
			if err != nil {
				return err
			}
			// Now resolve auth and the child tool, then verify identity before the
			// (potentially long) ghorg clone — honours the documented per-run principal
			// print and fails fast on a bad token without re-running discovery.
			token, source, err := resolveToken(cmd.Context())
			if err != nil {
				return err
			}
			announceTokenSource(cmd.ErrOrStderr(), source)
			if err := requireTool("ghorg", "https://github.com/gabrie30/ghorg#installation"); err != nil {
				return err
			}
			if err := verifyAndAnnouncePrincipal(cmd.Context(), cmd.ErrOrStderr(), token); err != nil {
				return err
			}
			if err := runMirror(cmd.Context(), execRun, sel, ws, token, mirror.Options{
				Branch:      branch,
				Concurrency: concurrency,
				CloneDepth:  cloneDepth,
				NoClean:     noClean,
				DryRun:      dryRun,
			}, reportOptions{toStdout: reportJSON, toFile: writeReport}, cmd.OutOrStdout(), cmd.ErrOrStderr()); err != nil {
				return err
			}
			// #29: drop a sidecar manifest in each --purpose snapshot so `workspaces
			// list/prune` get reliable structured metadata (purpose/branch/stamp/
			// owner) instead of parsing the ambiguous dir name. Owner is only known
			// here, from the selection.
			if snap != nil {
				snap.Owner = sel.Owner
			}
			return writeSnapshotManifest(ws, snap, dryRun)
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
	f.BoolVar(&reportJSON, "report-json", false, "after a successful, non-dry-run mirror, print a machine-readable JSON report (workspace, owner, repo count, requested branch, and per-repo branch status from the lockfile) to stdout")
	f.BoolVar(&writeReport, "write-report", false, "after a successful, non-dry-run mirror, write the JSON report to <workspace>/"+mirrorReportName)
	return cmd
}

// runMirror frames the mirror phase and delegates to the mirror package. It is
// the testable core of the mirror command (the Runner seam lets tests exercise
// it without ghorg installed).
//
// stdout carries exactly one machine-readable representation of the mirror: the
// bare workspace path by default, or the JSON report (which already includes the
// workspace) when --report-json is set. Emitting both would make the JSON
// unparseable, so the bare path is suppressed in report mode. Human banners and
// the ghorg child's own output always go to errOut (stderr) to keep stdout
// clean. The bare path prints before delegating, so it survives a later ghorg
// failure — callers must check the exit code, not stdout, for success; the JSON
// report, by contrast, is emitted only after a successful mirror.
func runMirror(ctx context.Context, run mirror.Runner, sel models.Selection, ws, token string, opts mirror.Options, report reportOptions, out, errOut io.Writer) error {
	opts.Workspace = ws
	// Reject an empty selection *before* printing anything to stdout. mirror.Mirror
	// also rejects it, but only after runMirror would already have emitted the bare
	// workspace path — misleading machine output for a run that never happens. An
	// empty lockfile usually means a misconfigured select (see §6), so fail loud
	// and clean here.
	if len(sel.Repos) == 0 {
		return errors.New("selection is empty — nothing to mirror; re-run `select` (a 0-repo select is an error unless --allow-empty)")
	}
	if !report.toStdout {
		fmt.Fprintln(out, ws)
	}
	banner(errOut, fmt.Sprintf("Mirroring %d repo(s) into %s", len(sel.Repos), ws))
	if err := mirror.Mirror(ctx, run, sel, token, opts); err != nil {
		return err
	}
	done(errOut, fmt.Sprintf("mirror complete → %s/%s", ws, sel.Owner))
	// goldfinger's own reconciliation line — the honest counterpart to ghorg's
	// "N new clones" summary and its per-repo "Could not checkout" fall-back noise.
	reportReconciliation(errOut, sel, ws, opts)
	return emitMirrorReport(sel, ws, opts, report, out, errOut)
}

// emitMirrorReport renders the mirror report to the requested sinks. It runs
// only after a successful, non-dry-run mirror: a report is never left claiming a
// clone that failed, and a dry-run (which clones nothing and may never create
// the workspace) produces no report — a --write-report into a non-existent
// workspace would otherwise fail.
func emitMirrorReport(sel models.Selection, ws string, opts mirror.Options, report reportOptions, out, errOut io.Writer) error {
	if opts.DryRun || (!report.toStdout && !report.toFile) {
		return nil
	}
	data, err := json.MarshalIndent(buildMirrorReport(sel, ws, opts), "", "  ")
	if err != nil {
		return fmt.Errorf("render mirror report: %w", err)
	}
	if report.toStdout {
		fmt.Fprintln(out, string(data))
	}
	if report.toFile {
		path := filepath.Join(ws, mirrorReportName)
		if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
			return fmt.Errorf("write mirror report: %w", err)
		}
		// WriteFile only applies the mode when creating the file, so a re-mirror
		// over a report left 0644 by an older goldfinger would keep the looser
		// mode; chmod makes 0600 hold on rewrite too.
		if err := os.Chmod(path, 0o600); err != nil {
			return fmt.Errorf("secure mirror report perms: %w", err)
		}
		done(errOut, "report written to "+path)
	}
	return nil
}

// writeSnapshotManifest persists the sidecar manifest for a --purpose snapshot
// after a successful, non-dry-run mirror. It is a no-op for the persistent
// default/--workspace case (snap == nil) and for a dry-run (which never creates a
// workspace to write into) — mirroring emitMirrorReport's "only on a real,
// successful mirror" posture, so no manifest is ever left describing a clone that
// did not happen.
func writeSnapshotManifest(ws string, snap *workspaceManifest, dryRun bool) error {
	if snap == nil || dryRun {
		return nil
	}
	return writeWorkspaceManifest(ws, *snap)
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
//
// For a --purpose snapshot it also returns a *workspaceManifest carrying the
// snapshot's identity (purpose, branch, stamp, creation time) so the caller can
// persist it after a successful mirror; Owner is left for the caller to fill from
// the selection. The manifest is nil for the --workspace and default cases, which
// are persistent workspaces, not managed snapshots.
func resolveWorkspace(workspace, purpose, branch string) (string, *workspaceManifest, error) {
	if workspace != "" && purpose != "" {
		return "", nil, errors.New("--workspace and --purpose are mutually exclusive")
	}
	if purpose != "" {
		if err := validatePurpose(purpose); err != nil {
			return "", nil, err
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", nil, fmt.Errorf("resolve home dir for workspace: %w", err)
		}
		now := nowFunc()
		stamp := now.Format(stampLayout)
		dir := purpose
		if branch != "" {
			// The real branch (with any slashes) still goes to ghorg; only the
			// dir-name component is sanitised so it stays a single safe segment.
			dir += "-" + sanitizeForDir(branch)
		}
		dir += "-" + stamp
		snap := &workspaceManifest{
			Version:   workspaceManifestVersion,
			Purpose:   purpose,
			Branch:    branch,
			Stamp:     stamp,
			CreatedAt: now,
		}
		return filepath.Join(home, "goldfinger", dir), snap, nil
	}
	if workspace == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", nil, fmt.Errorf("resolve home dir for workspace: %w", err)
		}
		return filepath.Join(home, "goldfinger"), nil, nil
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return "", nil, fmt.Errorf("resolve workspace path: %w", err)
	}
	return abs, nil, nil
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
