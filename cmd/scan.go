package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"

	"github.com/redscaresu/goldfinger/models"
	"github.com/redscaresu/goldfinger/selection"
	"github.com/spf13/cobra"
)

// skipReasonNotMirrored is recorded for a selected repo that is not present as a
// clone under the workspace — the honest counterpart to a match list, so a scan
// never silently omits a repo it could not search (issue #28).
const skipReasonNotMirrored = "not mirrored under workspace"

// skipReasonUnreadable is recorded for a repo that passed the on-disk clone gate
// but whose root could not then be opened or walked — a clone removed, swapped, or
// made unreadable between the gate and the walk (a TOCTOU race). Reporting it as
// NOT scanned (rather than a clean empty search) keeps the "no silent truncation"
// guarantee: a repo we could not actually read is never counted as searched.
const skipReasonUnreadable = "clone unreadable during scan"

// scanReport is the versioned, machine-readable result of a scan (issue #28). It
// is built from the authoritative lockfile plus a read-only walk of the local
// workspace — no git, no network, no re-discovery — so "the repos I selected" and
// "the repos I searched" are provably the same set, the same guarantee that makes
// apply valuable, applied to the read path.
type scanReport struct {
	Version      int    `json:"version"`
	Pattern      string `json:"pattern"`
	IgnoreCase   bool   `json:"ignoreCase"`
	FixedStrings bool   `json:"fixedStrings"`
	Workspace    string `json:"workspace"`
	Owner        string `json:"owner"`
	// Branch is the branch this workspace was mirrored from, read from the snapshot
	// manifest (`mirror --purpose` writes one) when present. It is the branch the
	// mirror *requested*; ghorg may have fallen back to a repo's default where the
	// branch was absent, so it carries the same caveat as the mirror report's
	// branchStatus. Absent (omitted) for the default/--workspace mirror, which has
	// no manifest — scan does not run git to guess it.
	Branch           string           `json:"branch,omitempty"`
	ReposInSelection int              `json:"reposInSelection"`
	ReposScanned     int              `json:"reposScanned"`
	ReposWithMatches int              `json:"reposWithMatches"`
	ReposNotScanned  int              `json:"reposNotScanned"`
	TotalMatches     int              `json:"totalMatches"`
	// Truncated is true when any cap trimmed the search: a per-repo match cap was
	// hit, or an oversize file was skipped. It is the report's honest "there may be
	// more" flag — the per-repo detail and skip reasons go to stderr (issue #28: no
	// silent truncation).
	Truncated bool             `json:"truncated"`
	Repos     []scanRepoResult `json:"repos"`
}

// scanRepoResult is one repo's slice of the report. A repo present on disk is
// Scanned with its Matches (possibly empty); one absent from the workspace is not
// Scanned and carries a SkipReason, so the set of selected repos is always fully
// accounted for.
type scanRepoResult struct {
	Repo       string      `json:"repo"` // owner/name
	Scanned    bool        `json:"scanned"`
	SkipReason string      `json:"skipReason,omitempty"`
	Matches    []scanMatch `json:"matches"`
	Truncated  bool        `json:"truncated,omitempty"`
}

// scanOptions bundles runScan's resolved inputs so it stays within the argument
// budget.
type scanOptions struct {
	pattern      string
	ignoreCase   bool
	fixedStrings bool
	asJSON       bool
	quiet        bool
}

func newScanCmd() *cobra.Command {
	var (
		selectionPath string
		name          string
		workspace     string
		ignoreCase    bool
		fixedStrings  bool
		asJSON        bool
	)
	cmd := &cobra.Command{
		Use:   "scan <pattern>",
		Short: "Search the mirrored selection locally and report matches as JSON",
		Long: "scan searches the repos of a frozen selection for a pattern and emits a " +
			"versioned JSON match report. It is the read/audit counterpart to apply's " +
			"write path: read-only EOL/CVE/supply-chain sweeps (\"where does " +
			"debian:bullseye appear?\", \"does any repo pin a poisoned name@version?\").\n\n" +
			"The search is entirely local: scan reads the lockfile for the exact repo " +
			"set, then greps the clones already on disk under the workspace (default " +
			"~/goldfinger; mirror first). It runs no git, opens no network connection, " +
			"and needs no token — so it burns zero GitHub rate limit. Multi-branch " +
			"(dev vs main) is two mirrors: `mirror --purpose audit --branch dev` and " +
			"`--branch main` into separate snapshots, then scan each.\n\n" +
			"A selected repo not present under the workspace is reported as not scanned " +
			"(with a reason), never silently dropped; and a scan trimmed by a size or " +
			"match cap — or by a file it could not read — sets `truncated` and warns on " +
			"stderr. Output is the JSON report on stdout under --json; the human summary " +
			"stays on stderr.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			quiet := quietRequested(cmd)
			errOut := humanErr(cmd)
			path, err := resolveSelectionPath(name, selectionPath)
			if err != nil {
				return err
			}
			sel, err := selection.Read(path)
			if err != nil {
				return err
			}
			// resolveWorkspace with no purpose/branch yields the default ~/goldfinger
			// or the given --workspace, made absolute — the same path mirror clones
			// into, so scan searches exactly where mirror wrote.
			ws, _, err := resolveWorkspace(workspace, "", "")
			if err != nil {
				return err
			}
			return runScan(sel, ws, scanOptions{
				pattern:      args[0],
				ignoreCase:   ignoreCase,
				fixedStrings: fixedStrings,
				asJSON:       asJSON,
				quiet:        quiet,
			}, cmd.OutOrStdout(), errOut)
		},
	}
	addSelectionFlags(cmd, &name, &selectionPath)
	f := cmd.Flags()
	f.StringVar(&workspace, "workspace", "", "workspace dir the selection was mirrored into (default ~/goldfinger; repos are read from <workspace>/<owner>)")
	f.BoolVarP(&ignoreCase, "ignore-case", "i", false, "case-insensitive match")
	f.BoolVarP(&fixedStrings, "fixed-strings", "F", false, "treat the pattern as a literal string, not a regular expression")
	f.BoolVar(&asJSON, "json", false, "emit the match report as JSON on stdout (the human summary stays on stderr)")
	return cmd
}

// runScan is scan's CLI core: it computes the report (compileScanPattern + a
// read-only walk of each selected repo's clone) and renders it. It runs no git
// and makes no network call — purely local search over the mirror. The reusable,
// I/O-free computeScanReport does the work; runScan only frames it for a human.
func runScan(sel models.Selection, ws string, o scanOptions, out, errOut io.Writer) error {
	errOut = quietWriter(errOut, o.quiet)
	if len(sel.Repos) > 0 {
		banner(errOut, fmt.Sprintf("Scanning %d repo(s) under %s/%s for %q", len(sel.Repos), ws, sel.Owner, o.pattern))
	}
	rep, err := computeScanReport(sel, ws, o)
	if err != nil {
		return err
	}
	if o.asJSON {
		if err := emitJSON(out, rep, o.quiet); err != nil {
			return err
		}
	} else if !o.quiet {
		renderScan(out, rep)
	}
	reportScanSummary(errOut, rep)
	return nil
}

// computeScanReport is scan's pure, reusable core, shared by the CLI (runScan)
// and the MCP scan tool. It validates the selection, compiles the pattern, and
// walks each selected repo's clone under the workspace, returning the versioned
// report. It performs only local filesystem reads — no git, no network, no token,
// and no writes to stdout/stderr — so it is safe to call from inside the MCP
// server (which owns the process stdio). It reads ONLY the selected repos under
// <workspace>/<owner>, never any other directory on disk (provable-same-set).
func computeScanReport(sel models.Selection, ws string, o scanOptions) (scanReport, error) {
	if len(sel.Repos) == 0 {
		return scanReport{}, fmt.Errorf("selection is empty — nothing to scan; re-run `select`")
	}
	re, err := compileScanPattern(o.pattern, o.ignoreCase, o.fixedStrings)
	if err != nil {
		return scanReport{}, fmt.Errorf("invalid pattern %q: %w", o.pattern, err)
	}
	// Search the selection in a stable, full-name order so the report (and the
	// per-repo slots seeded below) is deterministic regardless of lockfile order.
	repos := append([]models.Repo(nil), sel.Repos...)
	sort.Slice(repos, func(i, j int) bool { return repos[i].FullName() < repos[j].FullName() })

	rep := buildScanReport(sel, ws, o, repos)

	// Confine EVERY read within the workspace: open <ws> as an os.Root, then resolve
	// <owner> and each <name> through os.Root handles, which refuse any symlink that
	// escapes their parent. A symlinked owner or repo directory pointing outside the
	// workspace is therefore reported not-scanned, never followed — so scan reads
	// only the tree it reports on (provable-same-set), even under a raced or tampered
	// layout. Opening the root here (rather than joining a path string and gating with
	// a separate stat) is what closes the owner-level escape.
	wsRoot, err := os.OpenRoot(ws)
	if err != nil {
		// The workspace dir itself is absent or unopenable: nothing is mirrored here,
		// so every selected repo is not-on-disk (or, on a non-NotExist error, unreadable).
		markAllNotScanned(&rep, classifyScanOpenErr(err))
		return rep, nil
	}
	defer func() { _ = wsRoot.Close() }()

	// The owner directory must be a REAL directory inside the workspace: openVettedDir
	// rejects a symlink here (escaping OR in-workspace) and the Lstat→open swap race,
	// one level up from the per-repo check, so a raced owner symlink can't redirect the
	// whole scan into a different owner's tree.
	ownerRoot, reason := openVettedDir(wsRoot, sel.Owner)
	if reason != "" {
		markAllNotScanned(&rep, reason)
		return rep, nil
	}
	defer func() { _ = ownerRoot.Close() }()

	for i := range repos {
		res := &rep.Repos[i]
		repoRoot, reason := openScanClone(ownerRoot, repos[i].Name)
		if reason != "" {
			res.SkipReason = reason
			rep.ReposNotScanned++
			continue
		}
		scanned, err := searchTree(repoRoot, re)
		_ = repoRoot.Close()
		if err != nil {
			// The clone opened but its root could not be listed (a permission/identity
			// race after the gate). We searched nothing, so report it not-scanned rather
			// than a misleading scanned/0 — the honest coverage count (issue #28).
			res.SkipReason = skipReasonUnreadable
			rep.ReposNotScanned++
			continue
		}
		res.Scanned = true
		rep.ReposScanned++
		res.Matches = nonNilMatches(scanned.matches)
		res.Truncated = scanned.truncated
		if scanned.truncated {
			rep.Truncated = true
		}
		if len(scanned.matches) > 0 {
			rep.ReposWithMatches++
			rep.TotalMatches += len(scanned.matches)
		}
	}
	return rep, nil
}

// openVettedDir opens name within parent as a confined os.Root, or returns a skip
// reason when it is not a real directory we can safely descend. It rejects a
// symlinked or non-directory entry outright (Lstat describes the entry itself):
// ghorg never creates one, and os.Root would FOLLOW a symlink whose target stays
// inside the root, letting scan read a tree the selection never named. That leaves
// one narrow race — a real dir swapped for an in-workspace symlink between the Lstat
// and the open — which the post-open inode recheck closes: the opened directory must
// be the SAME object the Lstat vetted (os.SameFile), or its identity changed under
// us and we refuse it. A not-exist error classifies as not-mirrored; any other as
// unreadable, so the scan never claims coverage it lacks (issue #28). Used for both
// the owner directory and each repo clone, so the guarantee holds at every level of
// the <workspace>/<owner>/<name> path.
func openVettedDir(parent *os.Root, name string) (*os.Root, string) {
	li, err := parent.Lstat(name)
	if err != nil {
		return nil, classifyScanOpenErr(err)
	}
	if !li.IsDir() {
		return nil, skipReasonNotMirrored
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, classifyScanOpenErr(err)
	}
	di, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, classifyScanOpenErr(err)
	}
	if !os.SameFile(li, di) {
		_ = root.Close()
		return nil, skipReasonNotMirrored
	}
	return root, ""
}

// openScanClone resolves one selected repo's clone within the pinned owner root via
// openVettedDir (which handles symlink rejection and the swap race), then additionally
// requires a .git entry so a bare or half-written directory is reported not-mirrored.
// The .git entry may be a directory (a normal ghorg clone) or a regular file (a
// gitfile-linked worktree); a .git SYMLINK is rejected, since following it (Stat
// would) could count an unrelated git tree as this repo's clone.
func openScanClone(ownerRoot *os.Root, name string) (*os.Root, string) {
	repoRoot, reason := openVettedDir(ownerRoot, name)
	if reason != "" {
		return nil, reason
	}
	gi, err := repoRoot.Lstat(".git")
	if err != nil {
		_ = repoRoot.Close()
		return nil, classifyScanOpenErr(err)
	}
	if !gi.IsDir() && !gi.Mode().IsRegular() {
		_ = repoRoot.Close()
		return nil, skipReasonNotMirrored
	}
	return repoRoot, ""
}

// classifyScanOpenErr maps a filesystem open/stat error to the honest skip reason:
// a not-exist error means the repo was never mirrored here; any other error (a
// permission denial, or an entry removed/swapped mid-scan) means it exists but could
// not be read, which is a distinct, re-runnable coverage gap.
func classifyScanOpenErr(err error) string {
	if errors.Is(err, fs.ErrNotExist) {
		return skipReasonNotMirrored
	}
	return skipReasonUnreadable
}

// markAllNotScanned records reason on every repo slot and tallies them as
// not-scanned — used when the workspace or owner directory as a whole cannot be
// searched, so no repo is silently dropped from the count.
func markAllNotScanned(rep *scanReport, reason string) {
	for i := range rep.Repos {
		rep.Repos[i].SkipReason = reason
		rep.ReposNotScanned++
	}
}

// buildScanReport seeds the report with the selection-derived facts and one
// per-repo slot per entry of repos (default not-scanned, filled in by runScan).
// repos is the same ordered slice runScan then walks, so slot i and search i refer
// to the same repo. The snapshot manifest, when the workspace is a `mirror
// --purpose` snapshot, supplies the branch the mirror requested; the
// default/--workspace mirror has none and branch stays empty.
func buildScanReport(sel models.Selection, ws string, o scanOptions, repos []models.Repo) scanReport {
	rep := scanReport{
		Version:          scanReportVersion,
		Pattern:          o.pattern,
		IgnoreCase:       o.ignoreCase,
		FixedStrings:     o.fixedStrings,
		Workspace:        ws,
		Owner:            sel.Owner,
		ReposInSelection: len(sel.Repos),
		Repos:            make([]scanRepoResult, len(repos)),
	}
	if m, ok := readWorkspaceManifest(ws); ok {
		rep.Branch = m.Branch
	}
	for i, r := range repos {
		rep.Repos[i] = scanRepoResult{Repo: r.FullName(), Matches: []scanMatch{}}
	}
	return rep
}

// nonNilMatches returns m, or an empty (non-nil) slice, so a scanned repo's
// "matches" is always [] rather than null in the JSON.
func nonNilMatches(m []scanMatch) []scanMatch {
	if m == nil {
		return []scanMatch{}
	}
	return m
}

// renderScan writes the human-readable match report to out (stdout, so it can be
// piped/grepped): one line per match as path:line:text, grouped by repo. The
// framing summary lives on stderr (reportScanSummary).
func renderScan(out io.Writer, rep scanReport) {
	for _, r := range rep.Repos {
		if !r.Scanned || len(r.Matches) == 0 {
			continue
		}
		for _, m := range r.Matches {
			fmt.Fprintf(out, "%s\t%s:%d:%s\n", r.Repo, m.Path, m.Line, m.Text)
		}
	}
}

// reportScanSummary prints scan's terse outcome to errOut (stderr): coverage
// (scanned vs not-scanned), the match tally, and a caution when a cap truncated
// the search so a JSON-only reader's `truncated:true` is never the only signal.
func reportScanSummary(errOut io.Writer, rep scanReport) {
	done(errOut, fmt.Sprintf("scan complete: %d/%d repo(s) searched, %d match(es) in %d repo(s)",
		rep.ReposScanned, rep.ReposInSelection, rep.TotalMatches, rep.ReposWithMatches))
	if rep.ReposNotScanned > 0 {
		// A not-scanned repo has one of two reasons; word each honestly rather than
		// telling the operator to "mirror" a clone that was actually there but became
		// unreadable mid-scan. Counts come from the per-repo SkipReason (already in the
		// JSON), so no payload field is added.
		var notMirrored, unreadable int
		for _, r := range rep.Repos {
			switch r.SkipReason {
			case skipReasonNotMirrored:
				notMirrored++
			case skipReasonUnreadable:
				unreadable++
			}
		}
		if notMirrored > 0 {
			warn(errOut, fmt.Sprintf("%d selected repo(s) not on disk under %s/%s — mirror them first to include them",
				notMirrored, rep.Workspace, rep.Owner))
		}
		if unreadable > 0 {
			warn(errOut, fmt.Sprintf("%d selected repo(s) could not be read during the scan (removed or swapped after the on-disk check) — re-run scan to include them",
				unreadable))
		}
	}
	if rep.Truncated {
		warn(errOut, fmt.Sprintf("results truncated: a per-repo match cap (%d) was hit, a file over %d bytes was skipped, or a file/dir could not be read — narrow the pattern or inspect the repo directly",
			maxMatchesPerRepo, maxFileBytes))
	}
}
