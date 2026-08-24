// Package apply runs a change across a selection by shelling out to
// multi-gitter. goldfinger owns the selection; multi-gitter owns the
// clone→script→commit→push→PR.
package apply

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/redscaresu/goldfinger/models"
)

// tokenEnv is the environment variable multi-gitter reads its GitHub PAT from.
const tokenEnv = models.MultiGitterTokenEnvVar

// ErrInterrupted marks a run stopped by cancellation rather than by a failure.
//
// It exists so the CLI can tell an error that already KNOWS it was interrupted —
// and has something to add, like how much of the fleet a batched apply already
// pushed — from a delegate that was simply killed and reports "signal: killed",
// which says nothing useful. The CLI replaces the second and keeps the first.
var ErrInterrupted = errors.New("interrupted")

// multiGitterNeutralConfig is the config file goldfinger hands multi-gitter on
// every run, and it is deliberately not empty.
//
// An empty file would do nothing, because multi-gitter's --config does not
// REPLACE its static ~/.multi-gitter/config — it is layered above it. Both are
// applied by the same rule (cmd/config.go bindFlags, v0.63.1): a config value is
// written into a flag only "if the flag is not set". So the static file fills in
// every knob goldfinger leaves off argv, and an empty --config blocks nothing.
// Two of goldfinger's stated guarantees fall to that:
//
//   - Identity. `token:` in the static file is written into the --token flag, and
//     multi-gitter prefers that flag over the GITHUB_TOKEN goldfinger sets
//     (cmd/other.go getToken) — so it would open PRs as an account goldfinger
//     never resolved and never announced. `ssh-auth:` and `base-url:` are the
//     same defect by another route: a different credential, a different host.
//   - The exact set. goldfinger passes the lockfile as repeated --repo flags, but
//     `org:`/`user:`/`topic:`/`repo-search:` are DIFFERENT flags, so a static one
//     is added rather than overridden and multi-gitter targets their repos too —
//     opening PRs on repos that were never in the reviewed lockfile. `skip-repo:`
//     is the same hole inverted, silently dropping repos that were.
//   - The act itself, and how it is signed. `skip-pr:` is the worst of these:
//     multi-gitter only checks out the feature branch when it is opening a PR
//     (internal/multigitter/run.go), and GitHub has no remote-reference override,
//     so skip-pr pushes `HEAD` — still the BASE branch — to origin. A single
//     static line would turn a reviewed PR fanout into a direct push onto every
//     selected repo's default branch. `pr-auto-merge:` lands the PRs goldfinger
//     opens without the human who is supposed to press merge, and `api-push:` /
//     `git-type:` / `author-*:` each move commits onto a signing path other than
//     the one `--sign` named and the run announced.
//
// The fix follows from the same rule that causes it: a key present here marks its
// flag as set, so the static file can no longer fill it in. Every value below is
// therefore the neutral one — what that flag means when nobody asked for
// anything — chosen so the entry suppresses the static config without itself
// changing behaviour. `token: ""` is the load-bearing example: it blocks a static
// token while leaving getToken to fall through to the environment, which is where
// goldfinger's own token is and the only place a credential is allowed to travel.
//
// The inclusion rule is anything goldfinger is accountable for having stated: WHO
// the run acts as, WHICH repos it touches, WHAT it does to them, and every knob
// goldfinger models as a flag of its own and prints in the dry-run digest. That
// last category is the easiest to under-draw, because those keys look like the
// operator's business — but goldfinger passes most of them only when non-empty
// (--base-branch, --pr-body, --draft, --labels, --reviewers), so on any run that
// leaves one off, a static value silently fills it and the digest the human
// approved becomes a description of a different run.
//
// A key goldfinger already sets on argv is still listed. That is not redundant
// belt-and-braces, it is the same rule read the other way: bindFlags skips a flag
// that is already set, so on the runs where goldfinger passes the flag its own
// value wins and the entry is inert — and on the runs where it does NOT pass it
// (--api-push only for SignGitHub, --git-type only for SignLocal, --dry-run only
// for a dry run, and every conditional flag above) the entry is the only thing
// holding the default.
//
// What is deliberately left to the operator is the complement: knobs goldfinger
// does not model at all and never reports, so a host default contradicts nothing
// it said — merge-type (reachable only via pr-auto-merge, pinned off here),
// fetch-depth, concurrent, clone-dir, log level.
//
// One exception, and it is the reason this list is not simply "every flag".
// labels, reviewers, team-reviewers and assignees meet the rule and are still
// absent, because for them occupancy is not neutral: multi-gitter reads them with
// a helper that returns nil for an unset flag, and its GitHub layer treats nil as
// "leave alone" but a non-nil EMPTY slice as "make the PR match this" — i.e.
// remove every existing one (internal/scm/github/github.go setReviewers /
// setAssignees / setLabels). Claiming the key would therefore strip the reviewers
// CODEOWNERS requested and the labels a repo's automation added, on every run, on
// every host — a certain harm traded for a conditional one. State the residual
// exactly, because it is not merely additive: on runs where the operator named
// none of the four, a static value still reaches multi-gitter, and multi-gitter
// reconciles the PR TO that value — so it can put metadata goldfinger never
// reported onto the PR, and on a re-run that updates an existing PR (both
// CreatePullRequest and UpdatePullRequest call the same three setters) it can
// equally strip reviewers or labels added since. What it cannot touch is
// identity, the repo set, the action, or the signing path.
//
// Verified against multi-gitter v0.63.1 rather than reasoned about: with a
// hostile ~/.multi-gitter/config, each key above reaches multi-gitter's own
// validation (or, for base-branch, retargets a real dry run) when the config
// passed is empty, and does not when it is this one; argv still overrides this
// file; and a static `token:` stops winning while the environment still supplies
// goldfinger's. The `[""]` entries are the neutral form for a string slice, not a
// slice holding one empty string: bindFlags calls Set("") for the single element,
// and pflag parses that as an empty slice while still marking the flag set. That
// last property is also why the four keys in the exception above cannot be
// listed — for them multi-gitter reads "set to empty" as an instruction, not as
// an absence.
//
// The logging keys (log-file, log-level, log-format) are deliberately NOT
// occupied, and the reason is worth stating because the mirror side reaches the
// opposite conclusion about the same-shaped knob: GHORG_DEBUG is scrubbed there
// because ghorg prints the PAT under it, whereas multi-gitter installs a
// CensorFormatter that rewrites the token to "<TOKEN>" in every log line it emits
// (cmd/logging.go:69-78), so no log level discloses it. Nor can log-file blind
// the digest goldfinger shows the human before a real run: `run` writes the
// repo-counter block to r.Output (internal/multigitter/run.go:136-140), which is
// the `output` key occupied above, not to the logger. Do not "balance" the two
// sides by occupying these — the asymmetry is a real property of the delegates.
const multiGitterNeutralConfig = `# Written by goldfinger for a single run; every value is the flag's neutral
# default. Its purpose is to occupy these keys so that multi-gitter's static
# ~/.multi-gitter/config cannot fill them in — see apply.multiGitterNeutralConfig.

# Who the run acts as.
token: ""
username: ""
base-url: ""
platform: github
ssh-auth: false

# Which repos it touches. The lockfile arrives as repeated --repo flags; every
# key here is a DIFFERENT flag that would widen or narrow that set.
org: [""]
user: [""]
group: [""]
project: [""]
topic: [""]
skip-repo: [""]
repo-search: ""
code-search: ""
repo-include: ""
repo-exclude: ""
skip-forks: false
include-subgroups: false
fork: false
fork-owner: ""

# What it does to them, and how the commits are signed.
skip-pr: false
push-only: false
pr-auto-merge: false
dry-run: false
interactive: false
manual-commit: false
conflict-strategy: skip
push-option: [""]
api-push: false
git-type: go
author-name: ""
author-email: ""

# What goldfinger printed in the digest. Each of these has a goldfinger flag that
# is only passed when non-empty, so without an entry here the host supplies it on
# every run that leaves it off. labels/reviewers/team-reviewers/assignees belong
# to this group but are deliberately absent — see the "one exception" note in
# apply.multiGitterNeutralConfig; occupying them would strip PR metadata.
base-branch: ""
pr-body: ""
draft: false
max-reviewers: 0
max-team-reviewers: 0
output: "-"
`

// sleep pauses between batches, returning the context's error instead if the run
// is cancelled first. It is a package var so tests can stub it and not actually
// wait.
//
// Honouring the context matters more than it looks: --batch-pause exists to stay
// under GitHub's secondary rate limit, so it is set to a minute or more, and a
// plain time.Sleep would hold an interrupted run open for the rest of it. It used
// not to matter because Ctrl-C killed the process outright; now that goldfinger
// catches the signal to reap its delegates' process groups, every blocking step
// has to honour cancellation itself or the interrupt appears to be ignored.
var sleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// maxRepos caps how many repos we pass as repeated --repo flags. multi-gitter
// has no repo-list-file input, so a very large set would risk an over-length
// command line (E2BIG). Refuse loudly rather than truncate silently.
const maxRepos = 1000

// Runner executes an external command. It is the seam that lets Apply build and
// dispatch a multi-gitter invocation without multi-gitter installed in tests.
type Runner func(ctx context.Context, name string, args, env []string) ([]byte, error)

// Result is the observable output from an apply run that goldfinger owns. For a
// dry-run it carries the combined output captured from multi-gitter so cmd/ can
// summarize it honestly; live runs stream and usually leave Output nil.
type Result struct {
	Output []byte
}

// Apply runs spec's script across exactly the repos in s via multi-gitter. The
// token is passed through the child environment, never argv.
func Apply(ctx context.Context, run Runner, s models.Selection, spec models.ApplySpec, token string) (Result, error) {
	// Enforce the two charter invariants here, at the lowest exported execution
	// boundary — before writing the user script or invoking multi-gitter — so a
	// caller that bypasses the Cobra layer (e.g. a future MCP adapter calling
	// apply.Apply directly) cannot open PRs unconfirmed or unsigned. cmd/ still
	// guards these earlier for a friendlier CLI error; this is the backstop.
	//
	// (a) A live run (opens PRs) must be explicitly confirmed.
	if !spec.DryRun && !spec.Confirm {
		return Result{}, errors.New("refusing a live apply: DryRun is false but Confirm is false — a real run that opens PRs must be explicitly confirmed")
	}
	// (b) Every run must name a recognised signing mode — there is no default.
	if !models.IsValidSignMode(spec.Sign) {
		return Result{}, fmt.Errorf("invalid signing mode %q: must be one of %s (your GPG key), %s (GitHub-verified), or %s (unsigned)", spec.Sign, models.SignLocal, models.SignGitHub, models.SignNone)
	}

	if len(s.Repos) == 0 {
		return Result{}, errors.New("selection is empty — nothing to apply")
	}
	if len(s.Repos) > maxRepos {
		return Result{}, fmt.Errorf("selection has %d repos, above the %d-repo limit for a single apply (multi-gitter takes repos as command-line flags); narrow the selection", len(s.Repos), maxRepos)
	}

	scriptPath, cleanup, err := writeScript(spec.Script)
	if err != nil {
		return Result{}, err
	}
	defer cleanup()

	// Disarm multi-gitter's static config file (see multiGitterNeutralConfig for
	// why an empty file is not enough, and what it would otherwise cost).
	configPath, cfgCleanup, err := writeTempFile("goldfinger-mg-config-*.yaml", multiGitterNeutralConfig)
	if err != nil {
		return Result{}, err
	}
	defer cfgCleanup()

	// Strip EVERY credential-bearing variable, then map the resolved PAT onto
	// multi-gitter's own token var. Scrubbing the whole set — not just the source
	// PAT — is what makes the announced identity and the acting identity the same:
	// multi-gitter runs the operator's script as a grandchild of this process, and
	// a script that shells out to gh resolves an ambient GH_TOKEN ahead of the
	// GITHUB_TOKEN we set, so it would act as a different account entirely.
	//
	// git's own tracing knobs go too, because multi-gitter execs git directly
	// under --git-type=cmd (what --sign=local selects) with the PAT already in
	// the clone URL, and GIT_TRACE prints argv to a path the host chose. The
	// CensorFormatter that excuses multi-gitter's log keys from being occupied
	// cannot help: git writes that file itself, so multi-gitter never sees the
	// bytes to censor them.
	env := overrideEnv(os.Environ(), tokenEnv, token,
		append(models.CredentialEnvVars(), models.GitGrandchildEnvVars()...)...)

	// Split the selection into batches so PR creation stays under GitHub's
	// secondary rate limit (80 content-generating requests/min). Each batch is a
	// separate multi-gitter run over a subset of the lockfile; a pause between
	// batches spreads the writes out. multi-gitter's default conflict-strategy is
	// "skip", so a batch that fails partway (e.g. the hourly limit) is resumable
	// by re-running — already-created PRs are skipped.
	batches := chunk(s.Repos, spec.BatchSize)
	var result Result
	for i, repos := range batches {
		if i > 0 {
			if spec.BatchPause > 0 {
				if err := sleep(ctx, spec.BatchPause); err != nil {
					return result, interruptedAfter(i, len(batches), spec.DryRun)
				}
			}
			// Checked again, and unconditionally: --batch-pause=0 skips the wait
			// altogether, and even a wait that completed can race a signal that
			// landed as the timer fired. Without this the next batch starts on a
			// dead context — the delegate fails immediately, but with a generic
			// error that says nothing about how much of the fleet already shipped.
			if ctx.Err() != nil {
				return result, interruptedAfter(i, len(batches), spec.DryRun)
			}
		}
		sub := s
		sub.Repos = repos
		args := buildArgs(sub, spec, scriptPath, configPath)
		out, err := run(ctx, "multi-gitter", args, env)
		result.Output = appendBatchOutput(result.Output, out)
		if err != nil {
			if len(batches) > 1 {
				return result, fmt.Errorf("multi-gitter run (batch %d/%d): %w", i+1, len(batches), err)
			}
			return result, fmt.Errorf("multi-gitter run: %w", err)
		}
	}
	return result, nil
}

// interruptedAfter reports an apply stopped between batches, naming how far the
// fleet got. The count matters more than the word does, but it means two different
// things and the message must not conflate them: after a real run those batches
// have opened PRs that stopping goldfinger does not close, so the operator's next
// move depends on knowing which repos already carry one; after a dry run nothing
// was pushed, and the fact worth stating is that the digest covers part of the
// selection — a partial digest read as a whole one would under-report the blast
// radius of the live run it is meant to authorise.
func interruptedAfter(done, total int, dryRun bool) error {
	if dryRun {
		return fmt.Errorf("%w after batch %d/%d — nothing was pushed; the digest covers those repos only", ErrInterrupted, done, total)
	}
	return fmt.Errorf("%w after batch %d/%d — those PRs are already open", ErrInterrupted, done, total)
}

// appendBatchOutput concatenates one batch's captured output onto the run's.
//
// It deliberately does NOT bound the total. The runner bounds each batch (see
// cmd/exec.go: the tail, which is where multi-gitter's repo-counter block lands),
// so the dominant term — one runaway change command — is already capped; what is
// left grows with the batch COUNT, and tail-truncating here would drop whole
// earlier batches' counter blocks, silently reporting their repos as "unknown".
// A wrong digest is worse than a large one: the digest is what a human approves a
// real run against. Bounding this properly means summarising each batch as it
// finishes and merging digests rather than retaining raw bytes at all.
func appendBatchOutput(dst, src []byte) []byte {
	if len(src) == 0 {
		return dst
	}
	if len(dst) > 0 && dst[len(dst)-1] != '\n' {
		dst = append(dst, '\n')
	}
	return append(dst, src...)
}

// chunk splits repos into slices of at most size. A size <= 0 (or one large
// enough to hold everything) yields a single chunk — the unthrottled default.
func chunk(repos []models.Repo, size int) [][]models.Repo {
	if size <= 0 || size >= len(repos) {
		return [][]models.Repo{repos}
	}
	var out [][]models.Repo
	for start := 0; start < len(repos); start += size {
		end := start + size
		if end > len(repos) {
			end = len(repos)
		}
		out = append(out, repos[start:end])
	}
	return out
}

// buildArgs constructs the multi-gitter argv. Kept pure for unit testing.
func buildArgs(s models.Selection, spec models.ApplySpec, scriptPath, configPath string) []string {
	args := []string{"run", scriptPath, "--config=" + configPath}
	for _, r := range s.Repos {
		args = append(args, "--repo="+r.FullName())
	}
	args = append(args,
		"--branch="+spec.Branch,
		"--commit-message="+spec.CommitMessage,
		"--pr-title="+spec.PRTitle,
	)
	if spec.BaseBranch != "" {
		args = append(args, "--base-branch="+spec.BaseBranch)
	}
	if spec.PRBody != "" {
		args = append(args, "--pr-body="+spec.PRBody)
	}
	for _, l := range spec.Labels {
		args = append(args, "--labels="+l)
	}
	for _, rv := range spec.Reviewers {
		args = append(args, "--reviewers="+rv)
	}
	if spec.Draft {
		args = append(args, "--draft")
	}
	if spec.DryRun {
		args = append(args, "--dry-run")
	}
	args = append(args, signArgs(spec.Sign)...)
	return args
}

// signArgs maps a signing mode onto the multi-gitter flag that produces it.
// Apply rejects any unrecognised mode before this is reached, so the only mode
// that falls through to the default is SignNone — the intentional unsigned path.
func signArgs(mode string) []string {
	switch mode {
	case models.SignGitHub:
		// --api-push commits through the GitHub API, which signs with GitHub's
		// web-flow key (always "Verified"). GitHub-only, slower, unsuited to large
		// files.
		return []string{"--api-push"}
	case models.SignLocal:
		// --git-type=cmd makes multi-gitter commit via the real git binary:
		// `git add .` then `git commit --no-verify -m <msg>` (multi-gitter
		// v0.63.1, internal/git/cmdgit). It passes no -S and no --no-gpg-sign, so
		// git honours the operator's ~/.gitconfig commit.gpgsign / user.signingkey
		// and signs with their own GPG key. Verified empirically against v0.63.1.
		//
		// This holds ONLY because goldfinger never passes --author-name /
		// --author-email: multi-gitter reduces the commit's env to just
		// GIT_AUTHOR/COMMITTER_* when an author is set, stripping HOME/GPG_TTY and
		// breaking signing. Do not add author flags to buildArgs without
		// re-verifying.
		return []string{"--git-type=cmd"}
	default:
		// SignNone: no signing flag, multi-gitter's default go-git (unsigned) path.
		return nil
	}
}

// writeScript wraps the inline command in a POSIX script file, because
// multi-gitter takes a script *path* (which it runs in each repo's checkout),
// not an inline command with arguments.
func writeScript(cmd []string) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "goldfinger-apply-*.sh")
	if err != nil {
		return "", nil, fmt.Errorf("create script: %w", err)
	}
	quoted := make([]string, len(cmd))
	for i, a := range cmd {
		quoted[i] = shellQuote(a)
	}
	content := "#!/bin/sh\nexec " + strings.Join(quoted, " ") + "\n"
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", nil, fmt.Errorf("write script: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", nil, fmt.Errorf("close script: %w", err)
	}
	// 0700, not 0755: the script embeds the user's command, which may carry
	// sensitive paths — only the invoking user needs to read or execute it.
	if err := os.Chmod(f.Name(), 0o700); err != nil { //nolint:gosec // G302: the script must be executable (multi-gitter runs it), so 0700 is the minimum; it stays owner-only, never group/world.
		_ = os.Remove(f.Name())
		return "", nil, fmt.Errorf("chmod script: %w", err)
	}
	return f.Name(), func() { _ = os.Remove(f.Name()) }, nil
}

// writeTempFile creates a 0600 temp file matching pattern, writes content to it,
// and returns its path and a cleanup func. An empty content yields an empty file.
//
// Both packages use it to neutralise a delegate's ambient config discovery, and
// the two shapes are why it takes content rather than always writing nothing:
// ghorg is disarmed by a file that says nothing (its config file only ever ADDS
// keys), while multi-gitter needs a file that says something, because its
// explicit --config is layered ABOVE the static ~/.multi-gitter/config rather
// than replacing it — see multiGitterNeutralConfig.
func writeTempFile(pattern, content string) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", nil, fmt.Errorf("create temp file: %w", err)
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", nil, fmt.Errorf("write temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", nil, fmt.Errorf("close temp file: %w", err)
	}
	return f.Name(), func() { _ = os.Remove(f.Name()) }, nil
}

// shellQuote single-quotes a token so it survives POSIX sh word-splitting.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// overrideEnv returns base with key set to val exactly once, and every var named
// in drop removed. Any pre-existing key= entries are stripped before appending
// key=val: on Linux getenv returns the FIRST duplicate, so a value already
// present in the environment would otherwise win over ours. drop lets callers
// scrub the source PAT var so it never reaches the child.
//
// A drop entry ending in "*" is a PREFIX, matching every variable that starts
// with the text before it. Exact names are the norm and should stay the norm —
// a prefix scrubs variables nobody enumerated, which is only safe when the
// family is owned by someone else and genuinely grows (git's GIT_TRACE* knobs,
// see models.GitGrandchildEnvVars). Do not reach for it to save typing.
func overrideEnv(base []string, key, val string, drop ...string) []string {
	strip := map[string]bool{key: true}
	var prefixes []string
	for _, d := range drop {
		if p, ok := strings.CutSuffix(d, "*"); ok {
			prefixes = append(prefixes, p)
			continue
		}
		strip[d] = true
	}
	out := make([]string, 0, len(base)+1)
	for _, e := range base {
		name := e[:strings.IndexByte(e+"=", '=')]
		if strip[name] || hasAnyPrefix(name, prefixes) {
			continue
		}
		out = append(out, e)
	}
	return append(out, key+"="+val)
}

// hasAnyPrefix reports whether name starts with any of prefixes.
func hasAnyPrefix(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
