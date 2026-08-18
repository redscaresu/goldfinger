package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/redscaresu/goldfinger/apply"
	"github.com/redscaresu/goldfinger/models"
	"github.com/redscaresu/goldfinger/selection"
	"github.com/spf13/cobra"
)

func newApplyCmd() *cobra.Command {
	var (
		selectionPath string
		name          string
		branch        string
		baseBranch    string
		commitMessage string
		prTitle       string
		prBody        string
		prBodyFile    string
		labels        []string
		reviewers     []string
		sign          string
		draft         bool
		dryRun        bool
		confirm       bool
		batchSize     int
		batchPause    time.Duration
		planJSON      bool
		expectSHA     string
	)
	cmd := &cobra.Command{
		Use:   "apply [flags] -- command [args...]",
		Short: "Run a change across the selection and open PRs via multi-gitter",
		Long: "apply runs a change command in each selected repo and opens a PR per repo " +
			"via multi-gitter. It defaults to a dry-run; a real run additionally requires " +
			"--dry-run=false AND --confirm, and every run must state --sign.\n\n" +
			"Base branch routing: omit --base-branch to let each PR target its repo's own " +
			"default branch — goldfinger passes no base to multi-gitter, which resolves " +
			"each repo's LIVE default at run time, so a selection mixing (say) dev-default " +
			"and main-default repos routes correctly in one run. Pass --base-branch only to " +
			"force a single shared base across every repo (a repo lacking that branch then " +
			"errors). The dry-run banner prints the resolved base per repo so routing is " +
			"auditable before anything runs.",
		RunE: func(cmd *cobra.Command, args []string) error {
			quiet := quietRequested(cmd)
			errOut := humanErr(cmd)
			// Pure/local validation first — flags, mutual exclusions, the confirm
			// safety guard — so a bad invocation fails without resolving a token
			// (which can shell out to `gh`) or hitting the network.
			script := scriptArgs(cmd, args)
			if err := validateApply(applyValidation{
				branch:        branch,
				commitMessage: commitMessage,
				prTitle:       prTitle,
				sign:          sign,
				script:        script,
			}); err != nil {
				return err
			}
			expectDigest, err := validateExpectSelectionDigest(expectSHA)
			if err != nil {
				return err
			}
			// A long PR body is easier to supply from a file than a shell-quoted
			// flag; --pr-body-file loads it. The two body sources are mutually
			// exclusive so there's no ambiguity about which one wins.
			body, err := resolvePRBody(prBody, prBodyFile)
			if err != nil {
				return err
			}
			prBody = body
			// Safety guard: a real run opens PRs. Require an explicit --confirm
			// so it can never happen by omitting a flag.
			if !dryRun && !confirm {
				return errors.New("refusing to open PRs: re-run with --confirm to disable the dry-run safety, or keep --dry-run")
			}
			path, err := resolveSelectionPath(name, selectionPath)
			if err != nil {
				return err
			}
			sel, gotDigest, err := selection.ReadWithDigest(path)
			if err != nil {
				return err
			}
			// Bind the run to the exact lockfile a plan was reviewed against. This
			// refusal fires before any token/tool resolution or network call, so a
			// drifted selection costs nothing and — critically for an MCP caller —
			// the human-reviewed digest is what actually gates the PRs.
			if expectDigest != "" && gotDigest != expectDigest {
				return fmt.Errorf("selection digest mismatch: --expect-selection-sha256 %s but %s hashes to %s — the lockfile changed since the plan was made; re-review and re-run", expectDigest, path, gotDigest)
			}
			// Only now resolve auth — after every pure/local guard (flag combos,
			// PR-body, confirm, selection read) has passed. resolveToken is placed
			// before requireTool so a missing token fails independently of whether
			// multi-gitter is installed. A wrong principal is apply's costliest
			// mistake, so we verify and print it right before delegating.
			token, source, err := resolveToken(cmd.Context())
			if err != nil {
				return err
			}
			announceTokenSource(errOut, source)
			if err := requireTool("multi-gitter", "https://github.com/lindell/multi-gitter#installation"); err != nil {
				return err
			}
			// Surface the known-good-floor advisory HERE, not only in `doctor`: the
			// floor exists to make silent behavioural drift visible, and an operator
			// (or agent) who runs apply without a prior doctor would otherwise never
			// see it. Advisory only — apply never refuses on version (see the helper).
			warnMultiGitterFloor(cmd.Context(), errOut)
			if err := verifyAndAnnouncePrincipal(cmd.Context(), errOut, token); err != nil {
				return err
			}
			spec := models.ApplySpec{
				Branch:        branch,
				BaseBranch:    baseBranch,
				CommitMessage: commitMessage,
				PRTitle:       prTitle,
				PRBody:        prBody,
				Labels:        labels,
				Reviewers:     reviewers,
				Draft:         draft,
				DryRun:        dryRun,
				Confirm:       confirm,
				Script:        script,
				Sign:          sign,
				BatchSize:     batchSize,
				BatchPause:    batchPause,
			}
			run := execApplyRun
			if quiet {
				run = execApplyRunQuiet
			}
			return runApply(cmd.Context(), run, sel, spec, token, applyOutputOptions{planJSON: planJSON, quiet: quiet}, cmd.OutOrStdout(), errOut)
		},
	}
	addSelectionFlags(cmd, &name, &selectionPath)
	f := cmd.Flags()
	f.StringVar(&branch, "branch", "", "branch to commit changes to (required)")
	f.StringVar(&baseBranch, "base-branch", "", "base branch for the PR, e.g. main or dev (default: repo default branch)")
	f.StringVar(&commitMessage, "commit-message", "", "commit message (required)")
	f.StringVar(&prTitle, "pr-title", "", "pull request title (required)")
	f.StringVar(&prBody, "pr-body", "", "pull request body (mutually exclusive with --pr-body-file)")
	f.StringVar(&prBodyFile, "pr-body-file", "", "read the pull request body from a file (mutually exclusive with --pr-body)")
	f.StringArrayVar(&labels, "label", nil, "label to add to every PR (repeatable)")
	f.StringArrayVar(&reviewers, "reviewer", nil, "reviewer to request: user or org/team (repeatable)")
	f.StringVar(&sign, "sign", "", "how to sign commits (required): local (your GPG key via git), github (GitHub-verified via API), or none (unsigned)")
	f.BoolVar(&draft, "draft", false, "open PRs as drafts")
	f.BoolVar(&dryRun, "dry-run", true, "run without pushing or opening PRs (default; pass --dry-run=false for a real run)")
	f.BoolVar(&confirm, "confirm", false, "required alongside --dry-run=false to actually open PRs")
	f.IntVar(&batchSize, "batch-size", 0, "open PRs in batches of this many repos to stay under GitHub rate limits (0 = one run over the whole selection)")
	f.DurationVar(&batchPause, "batch-pause", 0, "pause between batches, e.g. 60s (only used with --batch-size)")
	f.StringVar(&expectSHA, "expect-selection-sha256", "", "refuse to run unless the selection lockfile's sha256 (over its exact bytes) matches this 64-char hex digest — binds an apply to the precise selection a plan was reviewed against (empty = no check)")
	f.BoolVar(&planJSON, "plan-json", false, "emit a machine-readable plan of what goldfinger will invoke (invocation metadata only, not the diff; command redacted to argv[0]) on stdout before delegating; supplements — does not replace — the dry-run status digest (on stderr; suppressed under --quiet, where the plan owns stdout)")
	return cmd
}

// warnMultiGitterFloor prints doctor's known-good-floor advisory at the point of
// action when the installed multi-gitter is below goldfinger's verified floor (or
// its version can't be read). It reuses doctor's probe and floor logic so the two
// surfaces can't drift: apply's --sign local (--git-type=cmd) and dry-run digest
// parsing were verified against multiGitterKnownGoodFloor, and an older binary may
// behave differently.
//
// It is deliberately advisory, never a gate: the floor is known-GOOD, not
// known-broken (an older multi-gitter may work fine, and the dry-run parser already
// fails safe if its output drifts), so blocking a possibly-working version would be
// user-hostile. errOut is already quiet-suppressed by the caller, so a --quiet run
// stays silent. A version that meets the floor prints nothing.
func warnMultiGitterFloor(ctx context.Context, errOut io.Writer) {
	_, version, ok := probeToolDefault(ctx, "multi-gitter")
	if !ok {
		// requireTool already confirmed presence; if the probe can't even find it now
		// (a race, or a goBinDir-only install), let the real run surface that rather
		// than emit a misleading "can't read version" line here.
		return
	}
	if line, warn := multiGitterFloorLine(version); warn {
		fmt.Fprintln(errOut, line)
	}
}

// multiGitterFloorLine formats the point-of-action advisory for a probed
// multi-gitter version, or ("", false) when the version meets the floor (nothing
// to say). It is the pure core of warnMultiGitterFloor, split out so the message
// assembly is unit-testable without a real probe; the below-floor / unreadable
// decision itself is versionFloorWarning's (shared with doctor, so the two can't
// drift).
func multiGitterFloorLine(version string) (string, bool) {
	detail, fix, warn := versionFloorWarning(version, multiGitterKnownGoodFloor)
	if !warn {
		return "", false
	}
	msg := "warning: multi-gitter " + detail
	if fix != "" {
		msg += " (" + fix + ")"
	}
	return msg, true
}

type applyOutputOptions struct {
	planJSON bool
	quiet    bool
}

// runApply frames the apply phase and delegates to the apply package. It is the
// testable core of the apply command.
//
// When planJSON is set, the machine-readable plan (what goldfinger will invoke) is
// written to out (stdout) before delegating — it supplements, and never replaces,
// the dry-run status digest (on stderr normally; relocated to stdout under
// --quiet, or suppressed when --quiet and --plan-json both claim stdout). It is
// not a metadata-only short-circuit: apply.Apply still runs.
func runApply(ctx context.Context, run apply.Runner, sel models.Selection, spec models.ApplySpec, token string, opts applyOutputOptions, out, errOut io.Writer) error {
	errOut = quietWriter(errOut, opts.quiet)
	mode := "LIVE — opening PRs"
	if spec.DryRun {
		mode = "dry-run — no push, no PRs"
	}
	baseLabel := spec.BaseBranch
	if baseLabel == "" {
		baseLabel = "each repo's default branch"
	}
	banner(errOut, fmt.Sprintf("Applying to %d repo(s) [%s] onto base %s", len(sel.Repos), mode, baseLabel))
	fmt.Fprintf(errOut, "  signing: %s\n", signTrust(spec.Sign))
	if spec.BatchSize > 0 && spec.BatchSize < len(sel.Repos) {
		fmt.Fprintf(errOut, "  throttling: batches of %d, pausing %s between them (stays under GitHub's 80 writes/min; the 500/hour ceiling still needs a re-run to resume)\n", spec.BatchSize, spec.BatchPause)
	}
	// Spell out the base per repo so the routing is auditable before anything
	// runs — this is exactly what a mixed dev/main selection needs to confirm
	// each PR lands on the right branch.
	for _, r := range sel.Repos {
		fmt.Fprintf(errOut, "  %s -> %s\n", r.FullName(), resolveBase(spec.BaseBranch, r))
	}
	// Without a global --base-branch, goldfinger passes no base to multi-gitter,
	// which targets each repo's *live* default at run time. The branches printed
	// above are the defaults recorded at selection time, so flag that they can
	// drift rather than presenting them as the guaranteed target.
	if spec.BaseBranch == "" {
		fmt.Fprintln(errOut, "  (branches shown are each repo's default recorded at selection; multi-gitter targets the live default at run time)")
	}
	// The plan goes to stdout (banners above went to stderr) so an agent can parse
	// it cleanly. It is emitted before delegating so it survives a later
	// multi-gitter failure, and it does not short-circuit the run — apply.Apply
	// still executes so the operator/agent also gets the dry-run status digest.
	if opts.planJSON {
		if err := emitJSON(out, buildApplyPlan(sel, spec), opts.quiet); err != nil {
			return err
		}
	}
	result, err := apply.Apply(ctx, run, sel, spec, token)
	if spec.DryRun {
		// The digest is the dry-run's machine result. In normal mode it goes to
		// stderr (the human stream) alongside a full-output drill-down log. Under
		// --quiet that stream is discarded, so the digest would vanish entirely —
		// relocate it to stdout so a machine run still learns would-change vs
		// no-change vs error, and skip the operator-only log file. With
		// --plan-json the plan already owns stdout, so the digest stays on the
		// (discarded) human stream rather than emitting a second, colliding
		// document.
		digestOut, writeLog := errOut, true
		if opts.quiet {
			writeLog = false
			if !opts.planJSON {
				digestOut = out
			}
		}
		if digestErr := printDryRunDigest(digestOut, sel.Repos, result.Output, writeLog); digestErr != nil {
			if err != nil {
				return errors.Join(err, digestErr)
			}
			return digestErr
		}
	}
	if err != nil {
		return err
	}
	done(errOut, "apply complete")
	return nil
}

func printDryRunDigest(w io.Writer, repos []models.Repo, output []byte, writeLog bool) error {
	digest := apply.SummarizeDryRunOutput(repos, output)
	repoWord := "repos"
	if digest.RepoCount == 1 {
		repoWord = "repo"
	}

	// Format drift (or an all-errors run): none of multi-gitter's known result
	// sections were found, so the per-repo buckets can't be trusted. Say so
	// plainly rather than print a confident digest that could relabel repos as
	// errored — the full run output is the source of truth here.
	if digest.Unparseable {
		fmt.Fprintf(w, "dry-run: %d %s — could not parse multi-gitter's result sections; per-repo status unavailable "+
			"(multi-gitter's output format may have changed, or every repo errored) — inspect the full run output\n",
			digest.RepoCount, repoWord)
		return emitFullRunLog(w, output, writeLog)
	}

	errorWord := "errors"
	if digest.Errored == 1 {
		errorWord = "error"
	}
	fmt.Fprintf(w, "dry-run: %d %s — %d would change, %d no-change, %d %s",
		digest.RepoCount, repoWord, digest.Changed, digest.Unchanged, digest.Errored, errorWord)
	if unknown := digest.RepoCount - digest.Changed - digest.Unchanged - digest.Errored; unknown > 0 {
		fmt.Fprintf(w, ", %d unknown", unknown)
	}
	fmt.Fprintln(w)
	for _, repo := range digest.Repos {
		if repo.Status == apply.DryRunError && repo.Detail != "" {
			fmt.Fprintf(w, "  %s   %s: %s\n", repo.Repo, repo.Status, repo.Detail)
			continue
		}
		fmt.Fprintf(w, "  %s   %s\n", repo.Repo, repo.Status)
	}
	return emitFullRunLog(w, output, writeLog)
}

// emitFullRunLog writes multi-gitter's full output to a 0600 temp file and prints
// its path — the operator drill-down aid on the human stream. A quiet machine run
// neither wants the file nor a path it cannot consume, so callers skip it there
// (and avoid a spurious exit-2 on an unwritable TMPDIR).
func emitFullRunLog(w io.Writer, output []byte, writeLog bool) error {
	if !writeLog {
		return nil
	}
	path, err := writeFullRunOutput(output)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "full run output: %s\n", path)
	return nil
}

func writeFullRunOutput(output []byte) (string, error) {
	f, err := os.CreateTemp("", "goldfinger-apply-output-*.log")
	if err != nil {
		return "", fmt.Errorf("create full run output file: %w", err)
	}
	path := f.Name()
	if _, err := f.Write(output); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("write full run output: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("secure full run output perms: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close full run output: %w", err)
	}
	return path, nil
}

// signTrust renders the signing mode and its trust model for the dry-run banner,
// so the operator sees exactly whose key vouches for each commit before a real
// run. Mirrors the mapping in apply.buildArgs.
func signTrust(mode string) string {
	switch mode {
	case models.SignLocal:
		return "local — commits signed with your GPG key (Verified on GitHub only if that public key is uploaded)"
	case models.SignGitHub:
		return "github — commits GitHub-verified (signed with GitHub's key via the API; GitHub-only, slower, unsuited to large files)"
	case models.SignNone:
		return "none — commits are UNSIGNED"
	default:
		return mode
	}
}

// resolvePRBody picks the PR body from either the inline --pr-body or the
// --pr-body-file path. Supplying both is an error; supplying neither yields an
// empty body.
func resolvePRBody(inline, path string) (string, error) {
	if path == "" {
		return inline, nil
	}
	if inline != "" {
		return "", errors.New("--pr-body and --pr-body-file are mutually exclusive")
	}
	b, err := os.ReadFile(path) //nolint:gosec // G304: path is the operator's own --pr-body-file argument, read to compose their PR body; reading a caller-named local file is the point.
	if err != nil {
		return "", fmt.Errorf("reading --pr-body-file: %w", err)
	}
	return string(b), nil
}

// resolveBase reports the branch a repo's PR will target: an explicit global
// --base-branch wins; otherwise multi-gitter uses the repo's own default
// branch. Falls back to a readable label when the lockfile lacks the default
// (e.g. an older selection or a hand-written one).
func resolveBase(globalBase string, r models.Repo) string {
	if globalBase != "" {
		return globalBase
	}
	if r.DefaultBranch != "" {
		return r.DefaultBranch
	}
	return "repo default"
}

// scriptArgs returns the command supplied after the `--` separator, or nil if
// no `--` was present.
func scriptArgs(cmd *cobra.Command, args []string) []string {
	dash := cmd.ArgsLenAtDash()
	if dash < 0 {
		return nil
	}
	return args[dash:]
}
