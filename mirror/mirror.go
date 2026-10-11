// Package mirror clones a selection into a local workspace by shelling out to
// ghorg. goldfinger owns the selection; ghorg owns the cloning.
package mirror

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/redscaresu/goldfinger/models"
)

// tokenEnv is the environment variable ghorg reads its GitHub PAT from.
const tokenEnv = models.GhorgTokenEnvVar

// allowedGhorgEnv is the whole list of GHORG_* variables the host may still pass
// to ghorg. Every other GHORG_* variable in goldfinger's environment is dropped
// before ghorg runs, and goldfinger then sets back only the ones it owns (the
// mapped token). It is an allowlist, not a deny-list, because a deny-list has to
// know every knob ghorg will ever read, and ghorg keeps adding them: v1.11.15
// added GHORG_REPO_FILTER_HOOK — an executable ghorg runs with the repo list on
// stdin, token-bearing clone URLs included, while it inherits GHORG_GITHUB_TOKEN —
// and v1.11.16 added GHORG_INCLUDE_SCM_DATA to feed that hook more. Neither was on
// the old four-category deny-list; both would have reached ghorg. Under an
// allowlist a knob ghorg ships tomorrow is dropped by default, and leaving one to
// the host is a decision someone has to write down here.
//
// What the host must not decide is unchanged, and it is worth keeping as the
// rule a new entry is checked against: which repos land (topics/prefix/regex/
// language/archived/forks/user-option, ignore and only paths, wiki and gists,
// dry-run, prune), where they land (path, output dir, scm-hostname nesting,
// backup), what is inside them (branch, depth, no-clean, protect-local,
// submodules), and where the token goes (scm type, base URL, clone protocol, ssh
// hostname — and GHORG_DEBUG, which prints the PAT three ways: cmd/root.go
// :271-273 and :358, viper.Debug() at :395, and the clone URL spew.Dump'd at
// git/git.go:43-50). A variable is only allowed when it can do none of those, and
// when in doubt it is dropped — so the exit-code, stats, pprof and trace knobs are
// dropped too: the first can turn a failed clone into exit 0, and the others write
// files into the workspace or the current directory.
//
// The scrub is one of two halves and cannot stand alone: ghorg re-creates any of
// these from its config file, which a missing environment variable does not
// suppress. The empty --config pinned in buildArgs is what closes that half, and
// the knobs with a flag that matters (layout, route, the filter hook) are pinned
// there too, since a ghorg flag outranks both the environment and the file.
var allowedGhorgEnv = map[string]bool{
	// How many clones run at once. goldfinger passes --concurrency when the
	// operator sets one, which outranks this; otherwise it only changes speed.
	"GHORG_CONCURRENCY": true,
	// A pause between clones, for operators throttling against a busy host. It
	// changes when each clone happens, not what it fetches.
	"GHORG_CLONE_DELAY_SECONDS": true,
	// How many repo-listing pages ghorg fetches in parallel — the knob for GitHub's
	// secondary rate limits on huge orgs. The listing is filtered to the lockfile
	// set by --target-repos-path regardless.
	"GHORG_GITHUB_REPO_LIST_CONCURRENCY": true,
	// Terminal colour of ghorg's own log lines.
	"GHORG_COLOR": true,
	// Fewer of ghorg's log lines. Fewer lines can only print less, never the token.
	"GHORG_QUIET": true,
	// Skips the end-of-run directory-size summary; output only.
	"GHORG_NO_DIR_SIZE": true,
}

// withoutHostGhorgEnv returns environ minus every GHORG_* variable that is not in
// allowedGhorgEnv. It runs before overrideEnv, which then sets the mapped token.
func withoutHostGhorgEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, e := range environ {
		name := e[:strings.IndexByte(e+"=", '=')]
		if strings.HasPrefix(name, "GHORG_") && !allowedGhorgEnv[name] {
			continue
		}
		out = append(out, e)
	}
	return out
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
	ignoreFile, ignoreCleanup, err := writeTempFile("goldfinger-ghorgignore-*", "")
	if err != nil {
		return err
	}
	defer ignoreCleanup()

	// Point ghorg at an empty config file. This is not belt-and-braces on the env
	// scrub below — it is the only thing that closes the channel, because ghorg's
	// config file OUTRANKS an absent environment variable rather than yielding to
	// it. ghorg resolves every GHORG_* knob through viper and then os.Setenv's the
	// result into its OWN environment (cmd/root.go getOrSetDefaults, v1.11.14), so
	// a variable goldfinger strips is simply re-created from the file. That turns
	// the config file into a bypass for both invariants: GHORG_GITHUB_APP_PEM_PATH
	// re-appears and ghorg switches to GitHub App auth, overwriting the client it
	// had already built from our PAT (scm/github.go:131-160) — so goldfinger would
	// announce one principal and ghorg clone as another — and GHORG_TOPICS /
	// GHORG_MATCH_* / GHORG_SKIP_* re-appear and narrow the set below the lockfile.
	// A flag outranks the file, so pinning one empty YAML shuts off every key at
	// once. It also closes the discovery order's quieter door: absent --config,
	// ghorg picks up a ./ghorg.yaml from whatever directory goldfinger happens to
	// be run in (cmd/root.go InitConfig).
	configFile, configCleanup, err := writeTempFile("goldfinger-ghorg-config-*.yaml", "")
	if err != nil {
		return err
	}
	defer configCleanup()

	// Disarm the host's ghorgonly the only way that works — by naming a path that
	// does not exist. ghorgonly is an ALLOWLIST, so the empty-file trick used for
	// ghorgignore inverts here: an empty ghorgonly matches no repo and ghorg would
	// clone nothing at all. ghorg skips the filter entirely when the path is absent
	// (cmd/repository_filter.go FilterByGhorgonly), and it never validates that the
	// path exists, so an absent one is the off switch. It has to be pointed away
	// rather than left alone: with no flag, ghorg falls back to
	// $HOME/.config/ghorg/ghorgonly, and if the operator happens to have one it
	// silently narrows the clone set below the lockfile.
	onlyFile, onlyCleanup, err := absentPath("goldfinger-ghorgonly-*")
	if err != nil {
		return err
	}
	defer onlyCleanup()

	args := buildArgs(s, namesFile, ignoreFile, configFile, onlyFile, opts)
	// Map the PAT onto ghorg's own token var, after dropping every GHORG_* var the
	// host set except the few in allowedGhorgEnv, and the credential set on top
	// (GH_TOKEN and friends are not GHORG_*). With the empty --config above — the
	// other half, covering the same names by a route this scrub cannot reach — no
	// credential but the mapped one reaches ghorg, it can only be spent against
	// GitHub over HTTPS, and the host can change neither the set nor the layout
	// out from under the lockfile.
	//
	// Then git's own variables, which the GHORG_* allowlist cannot reach because
	// they are not ghorg's: ghorg execs `git` and never sets cmd.Env, so git
	// inherits this environment with the PAT sitting in the clone URL ghorg just
	// built for it. See models.GitGrandchildEnvVars — that scrub is shared with
	// apply precisely because the exposure is a property of git, not of either
	// delegate.
	drop := append(models.CredentialEnvVars(), models.GitGrandchildEnvVars()...)
	env := overrideEnv(withoutHostGhorgEnv(os.Environ()), tokenEnv, token, drop...)
	if err := run(ctx, "ghorg", args, env); err != nil {
		return fmt.Errorf("ghorg clone %s: %w", s.Owner, err)
	}
	return nil
}

// buildArgs constructs the ghorg argv. Kept pure for unit testing.
func buildArgs(s models.Selection, namesFile, ignoreFile, configFile, onlyFile string, opts Options) []string {
	args := []string{
		"clone", s.Owner,
		"--clone-type=" + cloneType(s.OwnerType),
		"--target-repos-path=" + namesFile,
		"--ghorgignore-path=" + ignoreFile,
		// Deliberately a path that does not exist — see the call site in Mirror.
		"--ghorgonly-path=" + onlyFile,
		// The empty config file, pinned as a flag because a flag is the only thing
		// that outranks it — see the call site in Mirror for why an env scrub alone
		// does not reach it.
		"--config=" + configFile,
		// Pin the on-disk layout to <workspace>/<owner>/<repo> (see allowedGhorgEnv).
		// --output-dir=<owner> is ghorg's own default but stated explicitly so it
		// is deterministic, and --preserve-scm-hostname=false stops ghorg nesting
		// clones under a <hostname>/ subdir. A ghorg CLI flag overrides both the
		// matching env var and a ghorg config file, so together with the env scrub
		// the host cannot move the clones out from under the path goldfinger prints,
		// the mirror report, and the post-mirror reconciliation count.
		"--output-dir=" + s.Owner,
		"--preserve-scm-hostname=false",
		// Pin the route: GitHub, over HTTPS, at github.com — so the mapped token is
		// the credential the clone presents and github.com is the only host it is
		// presented to (see allowedGhorgEnv). ghorg re-Setenv's each of these from
		// argv inside cloneFunc, after its viper/env resolution has run, so a flag
		// beats both the host environment and the host config file. --base-url= is
		// an explicit clear, not an omission: passing it empty is what marks the
		// flag changed, and ghorg then falls back to its own github.com default.
		"--scm=github",
		"--base-url=",
		"--protocol=https",
		// Clear the repo filter hook (ghorg v1.11.15+): an executable ghorg would run
		// with every repo's token-bearing clone URL on stdin and GHORG_GITHUB_TOKEN in
		// its environment, free to drop or rewrite repos. The allowlist already
		// drops the variable and the empty --config the file key; the empty flag is
		// the third lock, and ghorg treats an empty hook as none
		// (cmd/repository_filter_hook.go FilterByHook). ghorg older than v1.11.15
		// rejects the flag as unknown, which fails closed.
		"--repo-filter-hook=",
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

// absentPath returns a path that is guaranteed not to exist, plus a cleanup func.
// It works by creating a private temp directory and naming a file inside it that
// is never created: goldfinger owns the directory, so nothing else can race a
// file into that path for the life of the run. Used for ghorg's --ghorgonly-path,
// where "off" is expressed as a path that isn't there (see Mirror).
func absentPath(pattern string) (path string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", pattern)
	if err != nil {
		return "", nil, fmt.Errorf("create temp dir: %w", err)
	}
	return filepath.Join(dir, "absent"), func() { _ = os.RemoveAll(dir) }, nil
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
