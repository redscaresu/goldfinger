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

// runCheck resolves live discovery for the selection's owner, applies the
// selection's own frozen filter, and reports drift against the lockfile. It is
// the testable core of the check command. It returns an exitError with code 1
// when drift is found so the process exits non-zero (for CI) without printing an
// error — the drift report itself has already gone to stdout.
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
	live := discovery.Select(raw, discovery.Filter{AllRepos: sel.Filter.AllRepos, Topics: sel.Filter.Topics})
	diff := discovery.Compare(sel.Repos, live, raw)

	// Owner type is compared here rather than inside discovery.Compare so that
	// function stays a pure repos-only diff. It matters because mirror passes the
	// owner type to ghorg as --clone-type; a user<->org flip breaks mirroring.
	ownerTypeMoved := sel.OwnerType != "" && liveOwnerType != "" && sel.OwnerType != liveOwnerType
	inSync := diff.Empty() && !ownerTypeMoved

	if o.asJSON {
		// Machine mode: the full report goes to stdout regardless of sync state, so
		// an agent gets one parseable object either way. The exit code (below) still
		// carries the domain signal, exactly as in human mode.
		if err := emitJSON(out, buildCheckReport(o.name, diff, ownerTypeMoved, sel.OwnerType, liveOwnerType, inSync)); err != nil {
			return err
		}
		if inSync {
			return nil
		}
		return exitError{code: 1}
	}

	if inSync {
		if !o.quiet {
			done(errOut, fmt.Sprintf("selection is in sync with live discovery (%d repo(s))", len(sel.Repos)))
		}
		return nil
	}
	if o.quiet {
		return exitError{code: 1}
	}
	renderDrift(out, sel, diff, ownerTypeMoved, liveOwnerType)
	return exitError{code: 1}
}

// buildCheckReport shapes the drift diff into the versioned --json payload. It is
// pure so the exact shape is trivially testable.
func buildCheckReport(name string, diff discovery.Diff, ownerTypeMoved bool, wasOwnerType, liveOwnerType string, inSync bool) checkReport {
	rep := checkReport{
		Version:            checkReportVersion,
		Name:               name,
		InSync:             inSync,
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
	if ownerTypeMoved {
		rep.OwnerTypeFlipped = &ownerTypeFlipJSON{From: wasOwnerType, To: liveOwnerType}
	}
	return rep
}

// renderDrift writes the human-readable drift report to out. Data goes to stdout
// so it can be piped or diffed; the banner/summary framing lives on stderr.
func renderDrift(out io.Writer, sel models.Selection, diff discovery.Diff, ownerTypeMoved bool, liveOwnerType string) {
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
	if ownerTypeMoved {
		fmt.Fprintf(out, "  ! owner type changed: %s -> %s\n", sel.OwnerType, liveOwnerType)
	}
	// Unchanged = locked repos still selected with an unchanged default branch.
	unchanged := len(sel.Repos) - len(diff.Removed) - len(diff.DefaultBranchMoved)
	fmt.Fprintf(out, "summary: %d unchanged, %d added, %d removed, %d branch moved\n",
		unchanged, len(diff.Added), len(diff.Removed), len(diff.DefaultBranchMoved))
}
