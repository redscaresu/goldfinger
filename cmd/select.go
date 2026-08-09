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

// defaultSelectionPath is where the lockfile lives unless --selection overrides.
const defaultSelectionPath = "goldfinger.selection"

// repoResolver is the slice of the GitHub client that `select` and `check` need.
// Defining it here (consumer side) lets the commands' logic be tested with a
// fake, no network required. It is satisfied by *client.Client.
type repoResolver interface {
	Verify(ctx context.Context) (string, error)
	ListRepos(ctx context.Context, owner string) ([]models.Repo, string, error)
}

// branchResolver extends repoResolver with the read-only branch lookup that
// `select --branch-presence` needs. `check` keeps to the narrower repoResolver,
// so it does not depend on a method it never calls.
type branchResolver interface {
	repoResolver
	BranchExists(ctx context.Context, owner, repo, branch string) (bool, error)
}

func newSelectCmd() *cobra.Command {
	var (
		t               targeting
		selectionPath   string
		name            string
		branchesToCheck []string
		allowEmpty      bool
		asJSON          bool
		list            bool
	)
	cmd := &cobra.Command{
		Use:   "select",
		Short: "Resolve an owner's repos by topic and freeze them as a selection",
		RunE: func(cmd *cobra.Command, args []string) error {
			quiet := quietRequested(cmd)
			errOut := humanErr(cmd)
			token, source, err := resolveToken(cmd.Context())
			if err != nil {
				return err
			}
			announceTokenSource(errOut, source)
			if err := validateTargeting(t); err != nil {
				return err
			}
			path, err := resolveSelectionPath(name, selectionPath)
			if err != nil {
				return err
			}
			c, err := client.New(token)
			if err != nil {
				return err
			}
			return runSelect(cmd.Context(), c, selectOpts{
				t:               t,
				branchesToCheck: branchesToCheck,
				selectionPath:   path,
				tool:            "goldfinger " + version,
				source:          source,
				allowEmpty:      allowEmpty,
				asJSON:          asJSON,
				list:            list,
				quiet:           quiet,
			}, cmd.OutOrStdout(), errOut)
		},
	}
	addTargetingFlags(cmd, &t)
	addSelectionFlags(cmd, &name, &selectionPath)
	cmd.Flags().StringArrayVar(&branchesToCheck, "branch-presence", nil,
		"record (read-only) whether this branch exists on each selected repo, frozen into the lockfile for a later `mirror --branch` (repeatable). Facts are recorded at selection time and can drift")
	cmd.Flags().BoolVar(&allowEmpty, "allow-empty", false,
		"write a lockfile even when the filter matches zero repos (default: a zero-repo result is an error, since it usually means a wrong token/owner/topic rather than an intended empty fleet)")
	cmd.Flags().BoolVar(&asJSON, "json", false,
		"emit the written selection as JSON on stdout (a {selectionPath, selection} wrapper; the selection is the full lockfile) instead of the plain repo list; banners stay on stderr")
	cmd.Flags().BoolVar(&list, "list", false,
		"echo every selected repo's full name on stdout (default: stdout stays terse — the count is on stderr and the full list is in the lockfile — so a large selection doesn't dump one line per repo)")
	return cmd
}

// selectOpts groups the inputs to runSelect so the core stays under the project's
// per-function parameter budget as select grows auth/empty-guard knobs.
type selectOpts struct {
	t               targeting
	branchesToCheck []string
	selectionPath   string
	tool            string
	source          string // resolved token source, for the auth banner + empty diagnostic
	allowEmpty      bool
	asJSON          bool
	list            bool
	quiet           bool
}

// selectJSONReport is the --json shape for select: a wrapper carrying the on-disk
// path plus the full lockfile object exactly as persisted. The lockfile is nested
// (not flattened) so `selection` is structurally identical to the written
// goldfinger.selection, and its own `version` field is the payload version — this
// is the one machine surface without a separate top-level version (issue #27 §4).
type selectJSONReport struct {
	SelectionPath string           `json:"selectionPath"`
	Selection     models.Selection `json:"selection"`
}

// runSelect resolves the target repos, filters them, annotates the selected set
// with any requested branch-presence facts, and writes the selection lockfile.
// It is the testable core of the select command.
func runSelect(ctx context.Context, r branchResolver, o selectOpts, out, errOut io.Writer) error {
	errOut = quietWriter(errOut, o.quiet)
	t := o.t
	banner(errOut, "Resolving selection for "+t.org)
	login, err := r.Verify(ctx)
	if err != nil {
		return fmt.Errorf("verifying token: %w", err)
	}
	announcePrincipal(errOut, login)
	repos, ownerType, err := r.ListRepos(ctx, t.org)
	if err != nil {
		return err
	}
	selected := discovery.Select(repos, discovery.Filter{AllRepos: t.allRepos, Topics: t.topics})

	// A zero-repo result is almost always a mistake (wrong token identity, wrong
	// owner, or a topic that matches nothing) rather than an intended empty fleet.
	// Failing here — instead of silently writing an empty lockfile that a later
	// mirror/apply would treat as "nothing to do" — turns a silent no-op into a
	// diagnosable error. --allow-empty is the escape hatch for the rare intended case.
	if len(selected) == 0 && !o.allowEmpty {
		return emptySelectionError(o.source, login, t)
	}

	// discovery.Select returns a nil slice for zero matches; with --allow-empty
	// that would serialise as "repos": null. Normalise to an empty slice so the
	// lockfile (and select --json) always carry a JSON array, which is what a
	// machine consumer expects.
	if selected == nil {
		selected = []models.Repo{}
	}

	branches := dedupeNonEmpty(o.branchesToCheck)
	if err := annotateBranchPresence(ctx, r, selected, branches, errOut); err != nil {
		return err
	}

	sel := models.Selection{
		Version:         models.SelectionVersion,
		Owner:           t.org,
		OwnerType:       ownerType,
		Filter:          models.SelectionFilter{AllRepos: t.allRepos, Topics: t.topics},
		ResolvedAt:      time.Now().UTC(),
		Tool:            o.tool,
		Repos:           selected,
		BranchesChecked: branches,
	}
	if err := selection.Write(o.selectionPath, sel); err != nil {
		return err
	}

	// Output shape, terse-by-default (issue #48 WS7): --json owns stdout with the
	// full wrapper; --list explicitly echoes the repo names (the old default, now
	// opt-in); quiet keeps stdout = the lockfile path (the machine capture
	// contract). Otherwise stdout stays empty — the list is already in the
	// lockfile and the count is on the stderr done() line — so an N-repo selection
	// doesn't force a driving agent to read N lines it can get from the file.
	switch {
	case o.asJSON:
		if err := emitJSON(out, selectJSONReport{SelectionPath: o.selectionPath, Selection: sel}, o.quiet); err != nil {
			return err
		}
	case o.list:
		for _, repo := range selected {
			fmt.Fprintln(out, repo.FullName())
		}
	case o.quiet:
		fmt.Fprintln(out, o.selectionPath)
	}
	done(errOut, fmt.Sprintf("%d repo(s) written to %s", len(selected), o.selectionPath))
	return nil
}

// emptySelectionError builds a diagnostic for a zero-repo result that names the
// identity and inputs in play and the usual causes, so an operator (or agent)
// can tell "wrong token" from "wrong topic" from "genuinely empty" at a glance
// rather than staring at a silent, empty lockfile.
func emptySelectionError(source, login string, t targeting) error {
	var filter string
	if t.allRepos {
		filter = "all repos"
	} else {
		filter = fmt.Sprintf("topic(s) %v", t.topics)
	}
	who := login
	if who == "" {
		who = "unknown"
	}
	return fmt.Errorf("no repositories matched %s for owner %q (authenticated as %s via %s). "+
		"Common causes: the token is a different identity than expected, the owner name is wrong, "+
		"or the topic matches nothing. Verify the identity above, check the owner, and confirm the "+
		"topic on GitHub. If an empty selection is genuinely intended, re-run with --allow-empty",
		filter, t.org, who, source)
}

// annotateBranchPresence records, for each selected repo, whether each requested
// branch exists — via read-only REST. A branch equal to the repo's own default
// is present by definition and short-circuits without an API call. selected is
// mutated in place. discovery.Select has already dropped archived repos, so only
// the live selection is probed.
func annotateBranchPresence(ctx context.Context, r branchResolver, selected []models.Repo, branches []string, errOut io.Writer) error {
	if len(branches) == 0 {
		return nil
	}
	banner(errOut, fmt.Sprintf("Recording branch presence (%d branch(es)) across %d repo(s)", len(branches), len(selected)))
	for i := range selected {
		repo := &selected[i]
		for _, b := range branches {
			if b == repo.DefaultBranch {
				recordPresence(repo, b, true)
				continue
			}
			has, err := r.BranchExists(ctx, repo.Owner, repo.Name, b)
			if err != nil {
				return err
			}
			recordPresence(repo, b, has)
		}
	}
	return nil
}

// recordPresence stores present for branch on repo, allocating the map lazily.
func recordPresence(repo *models.Repo, branch string, present bool) {
	if repo.BranchPresence == nil {
		repo.BranchPresence = make(map[string]bool)
	}
	repo.BranchPresence[branch] = present
}

// dedupeNonEmpty returns in with empty and duplicate entries removed, preserving
// first-seen order — so `--branch-presence dev --branch-presence dev` probes dev
// once and BranchesChecked lists it once.
func dedupeNonEmpty(in []string) []string {
	seen := make(map[string]bool, len(in))
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
