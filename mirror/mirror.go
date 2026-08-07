// Package mirror clones a selection into a local workspace by shelling out to
// ghorg. goldfinger owns the selection; ghorg owns the cloning.
package mirror

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/redscaresu/goldfinger/models"
)

// tokenEnv is the environment variable ghorg reads its GitHub PAT from.
const tokenEnv = "GHORG_GITHUB_TOKEN" //nolint:gosec // G101: this is the name of an env var, not a hardcoded credential.

// ambientGhorgEnv lists GHORG_* environment variables that would let host config
// silently change which repos get mirrored — by filtering the target set
// (topics/prefix/regex/language/archived/forks, or a pointed-at ghorgignore) or
// by pruning repos out of the workspace. goldfinger's guarantee is that the
// lockfile is the exact set, so these are scrubbed from ghorg's environment: the
// lockfile, not the host, decides what gets mirrored.
var ambientGhorgEnv = []string{
	"GHORG_TOPICS",
	"GHORG_MATCH_PREFIX",
	"GHORG_EXCLUDE_MATCH_PREFIX",
	"GHORG_MATCH_REGEX",
	"GHORG_EXCLUDE_MATCH_REGEX",
	"GHORG_GITHUB_FILTER_LANGUAGE",
	"GHORG_SKIP_ARCHIVED",
	"GHORG_SKIP_FORKS",
	"GHORG_IGNORE_PATH",
	"GHORG_PRUNE",
	"GHORG_PRUNE_NO_CONFIRM",
	"GHORG_PRUNE_UNTOUCHED_NO_CONFIRM",
}

// layoutGhorgEnv lists GHORG_* environment variables that change WHERE clones
// land on disk. goldfinger promises a fixed <workspace>/<owner>/<repo> layout —
// it prints that path on stdout, builds the mirror report against it, and
// reconciles the on-disk count against it — so the host must not be able to
// relocate the clones out from under that promise. These are scrubbed from
// ghorg's environment, and the GitHub-relevant knobs are additionally pinned in
// argv (--output-dir, --preserve-scm-hostname=false) so a ghorg config file
// (which a CLI flag still overrides, but an env scrub does not reach) can't move
// them either. GHORG_PRESERVE_DIRECTORY_STRUCTURE is GitLab-only, so scrubbing
// its env is enough (goldfinger only clones GitHub).
var layoutGhorgEnv = []string{
	"GHORG_OUTPUT_DIR",
	"GHORG_PRESERVE_SCM_HOSTNAME",
	"GHORG_PRESERVE_DIRECTORY_STRUCTURE",
}

// Runner executes an external command. It is the seam that lets Mirror build and
// dispatch a ghorg invocation without ghorg installed during tests.
type Runner func(ctx context.Context, name string, args, env []string) error

// Options are the passthrough knobs for a mirror run.
type Options struct {
	Workspace   string // ghorg --path (absolute); ghorg clones into <workspace>/<owner>
	Branch      string // ghorg --branch: checkout this branch in every repo ("" = each repo's default)
	Concurrency int    // 0 = ghorg default
	CloneDepth  int    // 0 = full history
	NoClean     bool   // skip ghorg's git-clean on existing clones, preserving local changes
	DryRun      bool
}

// Mirror clones exactly the repos in s into the workspace via ghorg. The token
// is passed through the child environment (never argv), so it cannot leak into
// process listings or error output.
func Mirror(ctx context.Context, run Runner, s models.Selection, token string, opts Options) error {
	if len(s.Repos) == 0 {
		return errors.New("selection is empty — nothing to mirror")
	}
	namesFile, cleanup, err := writeNamesFile(s.Repos)
	if err != nil {
		return err
	}
	defer cleanup()

	// Neutralise the default ~/.config/ghorg/ghorgignore (and any host one) by
	// pointing ghorg at an empty ignore file, so an ambient ghorgignore can't
	// silently drop repos from the lockfile set.
	ignoreFile, ignoreCleanup, err := writeEmptyFile("goldfinger-ghorgignore-*")
	if err != nil {
		return err
	}
	defer ignoreCleanup()

	args := buildArgs(s, namesFile, ignoreFile, opts)
	// Map the PAT onto ghorg's own token var, and strip the source var, any
	// ambient set-narrowing/pruning GHORG_* vars, and any layout-changing GHORG_*
	// vars, so the raw PAT never reaches ghorg and the host can change neither the
	// set nor the layout out from under the lockfile.
	drop := append([]string{models.TokenEnvVar}, ambientGhorgEnv...)
	drop = append(drop, layoutGhorgEnv...)
	env := overrideEnv(os.Environ(), tokenEnv, token, drop...)
	if err := run(ctx, "ghorg", args, env); err != nil {
		return fmt.Errorf("ghorg clone %s: %w", s.Owner, err)
	}
	return nil
}

// buildArgs constructs the ghorg argv. Kept pure for unit testing.
func buildArgs(s models.Selection, namesFile, ignoreFile string, opts Options) []string {
	args := []string{
		"clone", s.Owner,
		"--clone-type=" + cloneType(s.OwnerType),
		"--target-repos-path=" + namesFile,
		"--ghorgignore-path=" + ignoreFile,
		// Pin the on-disk layout to <workspace>/<owner>/<repo> (see layoutGhorgEnv).
		// --output-dir=<owner> is ghorg's own default but stated explicitly so it
		// is deterministic, and --preserve-scm-hostname=false stops ghorg nesting
		// clones under a <hostname>/ subdir. A ghorg CLI flag overrides both the
		// matching env var and a ghorg config file, so together with the env scrub
		// the host cannot move the clones out from under the path goldfinger prints,
		// the mirror report, and the post-mirror reconciliation count.
		"--output-dir=" + s.Owner,
		"--preserve-scm-hostname=false",
	}
	if opts.Workspace != "" {
		args = append(args, "--path="+opts.Workspace)
	}
	if opts.Branch != "" {
		args = append(args, "--branch="+opts.Branch)
	}
	if opts.Concurrency > 0 {
		args = append(args, "--concurrency="+strconv.Itoa(opts.Concurrency))
	}
	if opts.CloneDepth > 0 {
		args = append(args, "--clone-depth="+strconv.Itoa(opts.CloneDepth))
	}
	if opts.NoClean {
		args = append(args, "--no-clean")
	}
	if opts.DryRun {
		args = append(args, "--dry-run")
	}
	return args
}

// overrideEnv returns base with key set to val exactly once, and every var named
// in drop removed. Any pre-existing key= entries are stripped before appending
// key=val: on Linux getenv returns the FIRST duplicate, so a value already
// present in the environment would otherwise win over ours. drop lets callers
// scrub the source PAT var so it never reaches the child.
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

// cloneType maps a stored owner type to ghorg's --clone-type value.
func cloneType(ownerType string) string {
	if ownerType == models.OwnerOrganization {
		return "org"
	}
	return "user"
}

// writeNamesFile writes the repo names (basenames — ghorg matches on name) to a
// temp file for --target-repos-path, returning a cleanup func.
func writeNamesFile(repos []models.Repo) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "goldfinger-mirror-*.txt")
	if err != nil {
		return "", nil, fmt.Errorf("create names file: %w", err)
	}
	var b strings.Builder
	for _, r := range repos {
		b.WriteString(r.Name)
		b.WriteByte('\n')
	}
	if _, err := f.WriteString(b.String()); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", nil, fmt.Errorf("write names file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", nil, fmt.Errorf("close names file: %w", err)
	}
	return f.Name(), func() { _ = os.Remove(f.Name()) }, nil
}

// writeEmptyFile creates an empty temp file matching pattern and returns its
// path and a cleanup func. Used to hand ghorg an empty ghorgignore so no host
// ignore file can narrow the mirror set.
func writeEmptyFile(pattern string) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", nil, fmt.Errorf("create temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", nil, fmt.Errorf("close temp file: %w", err)
	}
	return f.Name(), func() { _ = os.Remove(f.Name()) }, nil
}
