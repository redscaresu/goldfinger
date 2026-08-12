package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
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

// branchResolver extends repoResolver with the read-only lookups `select` needs
// beyond a filter resolve: BranchExists for `--branch-presence`, and GetRepo for
// an explicit `--repo`/`--repos-from` selection. `check` keeps to the narrower
// repoResolver, so it does not depend on methods it never calls.
type branchResolver interface {
	repoResolver
	BranchExists(ctx context.Context, owner, repo, branch string) (bool, error)
	GetRepo(ctx context.Context, owner, name string) (models.Repo, string, error)
	OwnerType(ctx context.Context, owner string) (string, error)
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
			t, err = prepareTargeting(t)
			if err != nil {
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
	// Digest is the short repo-set fingerprint (issue #48 WS6): repo count plus a
	// short hash over the sorted repo full-names. An agent can compare it to a
	// later run's digest to confirm "same N repos" without diffing the full lockfile.
	Digest string `json:"digest"`
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
	sel, err := buildSelectionFromLive(ctx, r, o, login, errOut)
	if err != nil {
		return err
	}
	// select refreshes an existing lockfile in place, so overwrite is the intent.
	if err := selection.Write(o.selectionPath, sel, selection.WriteOptions{Overwrite: true}); err != nil {
		return err
	}
	selected := sel.Repos

	// Output shape, terse-by-default (issue #48 WS7): --json owns stdout with the
	// full wrapper; --list explicitly echoes the repo names (the old default, now
	// opt-in); quiet keeps stdout = the lockfile path (the machine capture
	// contract). Otherwise stdout stays empty — the list is already in the
	// lockfile and the count is on the stderr done() line — so an N-repo selection
	// doesn't force a driving agent to read N lines it can get from the file.
	// digest is the short repo-set fingerprint (issue #48 WS6): count is already
	// len(selected); hash lets a later run confirm "same N repos" cheaply.
	_, digest := selection.Digest(sel)
	switch {
	case o.asJSON:
		if err := emitJSON(out, selectJSONReport{SelectionPath: o.selectionPath, Selection: sel, Digest: digest}, o.quiet); err != nil {
			return err
		}
	case o.list:
		for _, repo := range selected {
			fmt.Fprintln(out, repo.FullName())
		}
	case o.quiet:
		fmt.Fprintln(out, o.selectionPath)
	}
	// The digest rides the done() line (stderr) after "written to <path>", so the
	// existing "N repo(s) written to <path>" phrasing stays intact for humans and
	// tests while adding the fingerprint an agent can note without re-reading.
	done(errOut, fmt.Sprintf("%d repo(s) written to %s (digest %s)", len(selected), o.selectionPath, digest))
	return nil
}

// buildSelectionFromLive resolves the owner's repos, applies the selection
// filter, records any requested branch-presence facts, and assembles the
// lockfile value — the output-free core shared by the CLI `select` command and
// the MCP `select` tool. It does NOT write the file or emit anything; the caller
// persists and reports. login (from the caller's own Verify) is used only to make
// the empty-selection diagnostic name the identity in play. errOut receives the
// branch-presence banner; MCP passes io.Discard.
func buildSelectionFromLive(ctx context.Context, r branchResolver, o selectOpts, login string, errOut io.Writer) (models.Selection, error) {
	if o.t.explicit() {
		return buildExplicitSelection(ctx, r, o, login, errOut)
	}
	t := o.t
	repos, ownerType, err := r.ListRepos(ctx, t.org)
	if err != nil {
		return models.Selection{}, err
	}
	selected := discovery.Select(repos, discovery.Filter{AllRepos: t.allRepos, Topics: t.topics})

	// A zero-repo result is almost always a mistake (wrong token identity, wrong
	// owner, or a topic that matches nothing) rather than an intended empty fleet.
	// Failing here — instead of silently writing an empty lockfile that a later
	// mirror/apply would treat as "nothing to do" — turns a silent no-op into a
	// diagnosable error. --allow-empty is the escape hatch for the rare intended case.
	if len(selected) == 0 && !o.allowEmpty {
		return models.Selection{}, emptySelectionError(o.source, login, t)
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
		return models.Selection{}, err
	}

	return models.Selection{
		Version:         models.SelectionVersion,
		Owner:           t.org,
		OwnerType:       ownerType,
		Filter:          models.SelectionFilter{AllRepos: t.allRepos, Topics: t.topics},
		ResolvedAt:      time.Now().UTC(),
		Tool:            o.tool,
		Repos:           selected,
		BranchesChecked: branches,
	}, nil
}

// buildExplicitSelection resolves an EXPLICIT selection: each named repo is
// looked up directly via read-only GET (r.GetRepo), rather than listing the owner
// and applying a filter. A 404 on any named repo is a hard error (surfaced from
// GetRepo) — an explicitly named repo that can't be resolved must fail loudly,
// like a wrong topic or owner does. Archived repos are kept: naming a repo is
// deliberate intent that overrides discovery.Select's archived-skip. The owner
// type is taken from the repos' own owner objects (all share --org), so an
// explicit selection records the same ownerType a filtered one would. The
// resulting lockfile carries Filter.Repos as the explicit-mode marker `check`
// keys on.
func buildExplicitSelection(ctx context.Context, r branchResolver, o selectOpts, login string, errOut io.Writer) (models.Selection, error) {
	t := o.t
	names := t.repos // already merged/normalised/deduped by resolveTargetRepos

	if len(names) == 0 && !o.allowEmpty {
		return models.Selection{}, emptyExplicitSelectionError(o.source, login, t.org)
	}

	selected := make([]models.Repo, 0, len(names))
	var ownerType string
	for _, name := range names {
		repo, ot, err := r.GetRepo(ctx, t.org, name)
		if err != nil {
			return models.Selection{}, err
		}
		// GetRepo follows GitHub's rename/transfer redirect, so a named repo can
		// come back under a different owner or name than requested. Freezing that
		// silently would break the single-owner model — Selection.Owner (--org)
		// would disagree with a repo's real owner, and mirror (which targets by
		// --org + basename) and apply (which targets by full name) would then act
		// on different repositories. Refuse it: an explicit selection freezes
		// exactly what was named. Case-insensitive, since GitHub owner/repo names
		// are.
		if !strings.EqualFold(repo.Owner, t.org) || !strings.EqualFold(repo.Name, name) {
			return models.Selection{}, fmt.Errorf(
				"named repo %s/%s resolved to %s — it was renamed or transferred; re-run select naming its current owner/name (owner must be --org %s)",
				t.org, name, repo.FullName(), t.org)
		}
		if ownerType == "" {
			ownerType = ot
		}
		selected = append(selected, repo)
	}

	// An explicit selection allowed to be empty (--allow-empty) has no repo to
	// read the owner type from; probe it directly so the lockfile still records a
	// valid, enum-constrained ownerType, consistent with an empty filtered one.
	if len(selected) == 0 {
		ot, err := r.OwnerType(ctx, t.org)
		if err != nil {
			return models.Selection{}, err
		}
		ownerType = ot
	}

	branches := dedupeNonEmpty(o.branchesToCheck)
	if err := annotateBranchPresence(ctx, r, selected, branches, errOut); err != nil {
		return models.Selection{}, err
	}

	return models.Selection{
		Version:         models.SelectionVersion,
		Owner:           t.org,
		OwnerType:       ownerType,
		Filter:          models.SelectionFilter{Repos: names},
		ResolvedAt:      time.Now().UTC(),
		Tool:            o.tool,
		Repos:           selected,
		BranchesChecked: branches,
	}, nil
}

// prepareTargeting validates the selection mode and, for an explicit selection,
// resolves --repo/--repos-from into the final normalised, deduped repo-basename
// list (stored back on targeting.repos). It is shared by the CLI `select` command
// and the MCP `select` tool so both enforce identical rules. It never re-reads the
// file after this: reposFrom stays set only so targeting.explicit() keeps
// reporting true for a zero-repo explicit set.
func prepareTargeting(t targeting) (targeting, error) {
	if err := validateTargeting(t); err != nil {
		return t, err
	}
	if t.explicit() {
		repos, err := resolveTargetRepos(t)
		if err != nil {
			return t, err
		}
		t.repos = repos
	}
	return t, nil
}

// resolveTargetRepos merges the --repo values with the basenames read from a
// --repos-from file (if any), normalises each against --org, and dedupes,
// preserving first-seen order. The result is the explicit repo-basename list.
func resolveTargetRepos(t targeting) ([]string, error) {
	raw := append([]string{}, t.repos...)
	if t.reposFrom != "" {
		fromFile, err := readReposFrom(t.reposFrom)
		if err != nil {
			return nil, err
		}
		raw = append(raw, fromFile...)
	}
	names := make([]string, 0, len(raw))
	for _, entry := range raw {
		name, err := normalizeRepoName(t.org, entry)
		if err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return dedupeRepoNames(names), nil
}

// dedupeRepoNames removes empty entries and case-insensitive duplicates,
// preserving first-seen order and casing. GitHub repo names are case-insensitive,
// so "svc" and "Svc" name the same repository; collapsing them here stops two
// entries resolving to the same repo and landing it in the selection twice.
func dedupeRepoNames(in []string) []string {
	seen := make(map[string]bool, len(in))
	var out []string
	for _, s := range in {
		if s == "" {
			continue
		}
		key := strings.ToLower(s)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, s)
	}
	return out
}

// readReposFrom reads a --repos-from file into a list of repo basenames, one per
// line, ignoring blank lines and #-comments (with surrounding whitespace trimmed).
func readReposFrom(path string) ([]string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // operator-supplied path, read-only
	if err != nil {
		return nil, fmt.Errorf("read --repos-from %s: %w", path, err)
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out, nil
}

// normalizeRepoName reduces an explicit repo entry to a bare basename under org.
// A bare name passes through. An "owner/name" entry is accepted only when owner
// equals --org (so a value pasted from GitHub still works), reduced to its
// basename; a different owner is a hard error — the single-owner model takes the
// owner from --org, not from the entry.
func normalizeRepoName(org, entry string) (string, error) {
	if !strings.Contains(entry, "/") {
		return entry, nil
	}
	owner, name, _ := strings.Cut(entry, "/")
	if !strings.EqualFold(owner, org) {
		return "", fmt.Errorf("repo %q names owner %q but --org is %q; list repos as bare names (owner comes from --org) or as %s/<name>", entry, owner, org, org)
	}
	if name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("invalid repo name %q: expected a bare name or %s/<name>", entry, org)
	}
	return name, nil
}

// emptyExplicitSelectionError builds a diagnostic for an explicit selection that
// resolved to zero repos — the --repos-from file was empty or all comments, or no
// --repo was given. It names the identity in play so the operator can tell a
// genuine mistake from an intended empty set (--allow-empty).
func emptyExplicitSelectionError(source, login, org string) error {
	who := login
	if who == "" {
		who = "unknown"
	}
	return fmt.Errorf("explicit selection for owner %q resolved to zero repos (authenticated as %s via %s). "+
		"The --repos-from file may be empty or all-comments, or no --repo was given. "+
		"If an empty selection is genuinely intended, re-run with --allow-empty", org, who, source)
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
