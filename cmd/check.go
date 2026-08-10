package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/redscaresu/goldfinger/client"
	"github.com/redscaresu/goldfinger/discovery"
	"github.com/redscaresu/goldfinger/models"
	"github.com/redscaresu/goldfinger/selection"
	"github.com/spf13/cobra"
)

func newCheckCmd() *cobra.Command {
	var (
		selectionPath string
		name          string
		asJSON        bool
	)
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Report whether a selection has drifted from live discovery",
		Long: "check re-runs discovery using the selection's own frozen filter and " +
			"diffs the result against the lockfile — reporting repos added, removed " +
			"(with a reason), whose default branch has moved, or whose owner type has " +
			"flipped since it was resolved.\n\n" +
			"It is read-only: it never rewrites the lockfile (re-run `select` to " +
			"refresh) and never touches mirror or apply. Exit status is 0 in sync, " +
			"1 when drift is found, and 2 on error — usable as a CI gate.",
		RunE: func(cmd *cobra.Command, args []string) error {
			quiet := quietRequested(cmd)
			errOut := humanErr(cmd)
			token, source, err := resolveToken(cmd.Context())
			if err != nil {
				return err
			}
			announceTokenSource(errOut, source)
			path, err := resolveSelectionPath(name, selectionPath)
			if err != nil {
				return err
			}
			sel, err := selection.Read(path)
			if err != nil {
				return err
			}
			c, err := client.New(token)
			if err != nil {
				return err
			}
			return runCheck(cmd.Context(), c, sel, checkOpts{name: name, asJSON: asJSON, quiet: quiet}, cmd.OutOrStdout(), errOut)
		},
	}
	addSelectionFlags(cmd, &name, &selectionPath)
	cmd.Flags().BoolVar(&asJSON, "json", false,
		"emit the drift report as JSON on stdout instead of the human table; exit code is unchanged (0 in sync, 1 drift, 2 error); banners stay on stderr")
	return cmd
}

// checkOpts groups the non-resolver inputs to runCheck. name is echoed into the
// JSON report only for a named selection (empty for a default/--selection run).
type checkOpts struct {
	name   string
	asJSON bool
	quiet  bool
}

// checkReport is the --json shape for check (issue #27 §2/§4). ownerTypeFlipped is
// a nullable object (a selection has exactly one owner), null when unchanged —
// not an array. name is omitted for a default/--selection run.
type checkReport struct {
	Version            int                `json:"version"`
	Name               string             `json:"name,omitempty"`
	InSync             bool               `json:"inSync"`
	Added              []string           `json:"added"`
	Removed            []removedJSON      `json:"removed"`
	DefaultBranchMoved []branchMovedJSON  `json:"defaultBranchMoved"`
	OwnerTypeFlipped   *ownerTypeFlipJSON `json:"ownerTypeFlipped"`
}

type removedJSON struct {
	Repo   string `json:"repo"`
	Reason string `json:"reason"`
}

type branchMovedJSON struct {
	Repo string `json:"repo"`
	From string `json:"from"`
	To   string `json:"to"`
}

type ownerTypeFlipJSON struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// checkResult is the structured outcome of a drift check: the repo diff, the
// owner-type comparison (both endpoints), and whether the two together mean
// in-sync. It is a pure value — no auth, no network, no rendering — so the CLI
// and the MCP layer share exactly one definition of "drift" and cannot diverge.
// Drift is deliberately NOT an error here: it is a successful result carrying
// InSync:false; only validation/auth/network/parse failures are errors. The
// exit-code-1-on-drift convention is a shell nicety applied by runCheck, not a
// property of this value.
type checkResult struct {
	Diff           discovery.Diff
	OwnerTypeMoved bool
	WasOwnerType   string
	LiveOwnerType  string
	InSync         bool
}

// computeCheckResult applies the selection's own frozen filter to the live repos
// and diffs the result against the lockfile, returning the structured drift. It
// is deliberately pure: the caller resolves auth and fetches the raw live repos,
// so this function has no I/O to stub and its verdict is trivially testable, and
// the MCP check tool can reuse it verbatim after doing its own Verify/ListRepos.
//
// Owner type is compared here rather than inside discovery.Compare so that
// function stays a pure repos-only diff. It matters because mirror passes the
// owner type to ghorg as --clone-type; a user<->org flip breaks mirroring.
func computeCheckResult(sel models.Selection, raw []models.Repo, liveOwnerType string) checkResult {
	live := discovery.Select(raw, discovery.Filter{AllRepos: sel.Filter.AllRepos, Topics: sel.Filter.Topics})
	diff := discovery.Compare(sel.Repos, live, raw)
	ownerTypeMoved := sel.OwnerType != "" && liveOwnerType != "" && sel.OwnerType != liveOwnerType
	return checkResult{
		Diff:           diff,
		OwnerTypeMoved: ownerTypeMoved,
		WasOwnerType:   sel.OwnerType,
		LiveOwnerType:  liveOwnerType,
		InSync:         diff.Empty() && !ownerTypeMoved,
	}
}

// runCheck resolves live discovery for the selection's owner, applies the
// selection's own frozen filter, and reports drift against the lockfile. It is
// the testable core of the check command. It returns an exitError with code 1
// when drift is found so the process exits non-zero (for CI) without printing an
// error — the drift report itself has already gone to stdout. Auth resolution
// (Verify/principal) stays here in the caller, not in computeCheckResult, so that
// value can remain a pure drift computation.
func runCheck(ctx context.Context, r repoResolver, sel models.Selection, o checkOpts, out, errOut io.Writer) error {
	errOut = quietWriter(errOut, o.quiet)
	banner(errOut, "Checking selection for "+sel.Owner+" against live discovery")
	login, err := r.Verify(ctx)
	if err != nil {
		return fmt.Errorf("verifying token: %w", err)
	}
	announcePrincipal(errOut, login)
	raw, liveOwnerType, err := r.ListRepos(ctx, sel.Owner)
	if err != nil {
		return err
	}
	res := computeCheckResult(sel, raw, liveOwnerType)

	if o.asJSON {
		// Machine mode: the full report goes to stdout regardless of sync state, so
		// an agent gets one parseable object either way. The exit code (below) still
		// carries the domain signal, exactly as in human mode.
		if err := emitJSON(out, buildCheckReport(o.name, res), o.quiet); err != nil {
			return err
		}
		if res.InSync {
			return nil
		}
		return exitError{code: 1}
	}

	if res.InSync {
		if !o.quiet {
			done(errOut, fmt.Sprintf("selection is in sync with live discovery (%d repo(s))", len(sel.Repos)))
		}
		return nil
	}
	if o.quiet {
		return exitError{code: 1}
	}
	renderDrift(out, sel, res)
	return exitError{code: 1}
}

// buildCheckReport shapes the structured drift result into the versioned --json
// payload. It is pure and consumes checkResult alone (plus the display name), so
// the CLI and the MCP layer emit an identical report from the same value.
func buildCheckReport(name string, res checkResult) checkReport {
	diff := res.Diff
	rep := checkReport{
		Version:            checkReportVersion,
		Name:               name,
		InSync:             res.InSync,
		Added:              make([]string, 0, len(diff.Added)),
		Removed:            make([]removedJSON, 0, len(diff.Removed)),
		DefaultBranchMoved: make([]branchMovedJSON, 0, len(diff.DefaultBranchMoved)),
	}
	for _, a := range diff.Added {
		rep.Added = append(rep.Added, a.FullName())
	}
	for _, rm := range diff.Removed {
		rep.Removed = append(rep.Removed, removedJSON{Repo: rm.Repo.FullName(), Reason: rm.Reason})
	}
	for _, bc := range diff.DefaultBranchMoved {
		rep.DefaultBranchMoved = append(rep.DefaultBranchMoved, branchMovedJSON{Repo: bc.Repo.FullName(), From: bc.Was, To: bc.Now})
	}
	if res.OwnerTypeMoved {
		rep.OwnerTypeFlipped = &ownerTypeFlipJSON{From: res.WasOwnerType, To: res.LiveOwnerType}
	}
	return rep
}

// renderDrift writes the human-readable drift report to out. Data goes to stdout
// so it can be piped or diffed; the banner/summary framing lives on stderr.
func renderDrift(out io.Writer, sel models.Selection, res checkResult) {
	diff := res.Diff
	fmt.Fprintf(out, "selection drift vs live discovery (resolved %s):\n", sel.ResolvedAt.Format(time.RFC3339))
	for _, r := range diff.Added {
		fmt.Fprintf(out, "  + %s\tadded, matches the selection filter\n", r.FullName())
	}
	for _, rm := range diff.Removed {
		fmt.Fprintf(out, "  - %s\t%s\n", rm.Repo.FullName(), rm.Reason)
	}
	for _, bc := range diff.DefaultBranchMoved {
		fmt.Fprintf(out, "  ~ %s\tdefault branch moved: %s -> %s\n", bc.Repo.FullName(), bc.Was, bc.Now)
	}
	if res.OwnerTypeMoved {
		fmt.Fprintf(out, "  ! owner type changed: %s -> %s\n", res.WasOwnerType, res.LiveOwnerType)
	}
	// Unchanged = locked repos still selected with an unchanged default branch.
	unchanged := len(sel.Repos) - len(diff.Removed) - len(diff.DefaultBranchMoved)
	fmt.Fprintf(out, "summary: %d unchanged, %d added, %d removed, %d branch moved\n",
		unchanged, len(diff.Added), len(diff.Removed), len(diff.DefaultBranchMoved))
}
