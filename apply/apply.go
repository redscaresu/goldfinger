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
const tokenEnv = "GITHUB_TOKEN"

// sleep pauses between batches. It is a package var so tests can stub it and not
// actually wait.
var sleep = time.Sleep

// maxRepos caps how many repos we pass as repeated --repo flags. multi-gitter
// has no repo-list-file input, so a very large set would risk an over-length
// command line (E2BIG). Refuse loudly rather than truncate silently.
const maxRepos = 1000

// Runner executes an external command. It is the seam that lets Apply build and
// dispatch a multi-gitter invocation without multi-gitter installed in tests.
type Runner func(ctx context.Context, name string, args, env []string) error

// Apply runs spec's script across exactly the repos in s via multi-gitter. The
// token is passed through the child environment, never argv.
func Apply(ctx context.Context, run Runner, s models.Selection, spec models.ApplySpec, token string) error {
	// Enforce the two charter invariants here, at the lowest exported execution
	// boundary — before writing the user script or invoking multi-gitter — so a
	// caller that bypasses the Cobra layer (e.g. a future MCP adapter calling
	// apply.Apply directly) cannot open PRs unconfirmed or unsigned. cmd/ still
	// guards these earlier for a friendlier CLI error; this is the backstop.
	//
	// (a) A live run (opens PRs) must be explicitly confirmed.
	if !spec.DryRun && !spec.Confirm {
		return errors.New("refusing a live apply: DryRun is false but Confirm is false — a real run that opens PRs must be explicitly confirmed")
	}
	// (b) Every run must name a recognised signing mode — there is no default.
	if !models.IsValidSignMode(spec.Sign) {
		return fmt.Errorf("invalid signing mode %q: must be one of %s (your GPG key), %s (GitHub-verified), or %s (unsigned)", spec.Sign, models.SignLocal, models.SignGitHub, models.SignNone)
	}

	if len(s.Repos) == 0 {
		return errors.New("selection is empty — nothing to apply")
	}
	if len(s.Repos) > maxRepos {
		return fmt.Errorf("selection has %d repos, above the %d-repo limit for a single apply (multi-gitter takes repos as command-line flags); narrow the selection", len(s.Repos), maxRepos)
	}

	scriptPath, cleanup, err := writeScript(spec.Script)
	if err != nil {
		return err
	}
	defer cleanup()

	// Point multi-gitter at an empty config file so its default config-file
	// discovery (which can carry its own repo/org selection and filters) can't
	// override the exact lockfile set goldfinger passes as --repo flags.
	configPath, cfgCleanup, err := writeEmptyFile("goldfinger-mg-config-*.yaml")
	if err != nil {
		return err
	}
	defer cfgCleanup()

	// Map the PAT onto multi-gitter's own token var, and strip the source var
	// so the raw PAT never reaches multi-gitter or the user's apply script.
	env := overrideEnv(os.Environ(), tokenEnv, token, models.TokenEnvVar)

	// Split the selection into batches so PR creation stays under GitHub's
	// secondary rate limit (80 content-generating requests/min). Each batch is a
	// separate multi-gitter run over a subset of the lockfile; a pause between
	// batches spreads the writes out. multi-gitter's default conflict-strategy is
	// "skip", so a batch that fails partway (e.g. the hourly limit) is resumable
	// by re-running — already-created PRs are skipped.
	batches := chunk(s.Repos, spec.BatchSize)
	for i, repos := range batches {
		if i > 0 && spec.BatchPause > 0 {
			sleep(spec.BatchPause)
		}
		sub := s
		sub.Repos = repos
		args := buildArgs(sub, spec, scriptPath, configPath)
		if err := run(ctx, "multi-gitter", args, env); err != nil {
			if len(batches) > 1 {
				return fmt.Errorf("multi-gitter run (batch %d/%d): %w", i+1, len(batches), err)
			}
			return fmt.Errorf("multi-gitter run: %w", err)
		}
	}
	return nil
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
		f.Close()
		os.Remove(f.Name())
		return "", nil, fmt.Errorf("write script: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", nil, fmt.Errorf("close script: %w", err)
	}
	// 0700, not 0755: the script embeds the user's command, which may carry
	// sensitive paths — only the invoking user needs to read or execute it.
	if err := os.Chmod(f.Name(), 0o700); err != nil {
		os.Remove(f.Name())
		return "", nil, fmt.Errorf("chmod script: %w", err)
	}
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}

// writeEmptyFile creates an empty temp file matching pattern and returns its
// path and a cleanup func. Used to hand multi-gitter an empty --config so its
// default config-file discovery can't override the lockfile selection.
func writeEmptyFile(pattern string) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", nil, fmt.Errorf("create temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", nil, fmt.Errorf("close temp file: %w", err)
	}
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}

// shellQuote single-quotes a token so it survives POSIX sh word-splitting.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// overrideEnv returns base with key set to val exactly once, and every var named
// in drop removed. Any pre-existing key= entries are stripped before appending
// key=val: on Linux getenv returns the FIRST duplicate, so a value already
// present (e.g. CI's own GITHUB_TOKEN) would otherwise win over ours. drop lets
// callers scrub the source PAT var so it never reaches the child.
func overrideEnv(base []string, key, val string, drop ...string) []string {
	strip := map[string]bool{key: true}
	for _, d := range drop {
		strip[d] = true
	}
	out := make([]string, 0, len(base)+1)
	for _, e := range base {
		name := e[:strings.IndexByte(e+"=", '=')]
		if !strip[name] {
			out = append(out, e)
		}
	}
	return append(out, key+"="+val)
}
