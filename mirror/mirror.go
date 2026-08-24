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

// The four GHORG_* lists below are the whole rule for what the host may not
// decide, and it is worth stating so the next reader can check the rule rather
// than the list. A knob is scrubbed if it changes which repos land
// (ambientGhorgEnv), where they land (layoutGhorgEnv), what is inside them
// (contentGhorgEnv), or where the token is spent and which credential is
// presented (authRouteGhorgEnv) — plus the credential set itself, which lives in
// models.CredentialEnvVars because apply needs it too.
//
// Everything else ghorg reads is left to the host deliberately. Concurrency,
// clone delay, colour, quiet and stats change how the mirror runs, not what it
// produces. Note that GHORG_DEBUG is NOT one of them despite reading like one —
// it is in authRouteGhorgEnv because what it adds to the output is the token; a
// verbosity knob is only safe to leave alone once you have checked what the extra
// verbosity contains. The other forges' knobs (GitLab groups, Bitbucket
// credentials, the per-forge insecure-client switches) are unreachable once
// --scm=github is pinned. GHORG_EXIT_CODE_ON_CLONE_ISSUES can make ghorg exit 0 on a partial
// clone, but it cannot make one look complete: runMirror reconciles the
// workspace against the lockfile by stat, and reports coverage from that rather
// than from ghorg's exit status.
//
// Four knobs look like they belong in contentGhorgEnv and do not, because each
// one enlarges .git without touching a byte the working tree holds — and the
// working tree is all goldfinger's consumers ever read, since scan skips .git
// wholesale and nothing here runs git against a clone:
//
//   - GHORG_FETCH_GIT_LFS runs `git lfs fetch --all` (git/git.go:367-368), which
//     populates .git/lfs/objects. Materialising those into the tree would take
//     `git lfs pull`/`checkout`, which ghorg never runs, so pointer files stay
//     pointer files either way.
//   - GHORG_FETCH_ALL adds a `git fetch --all`; it moves remote-tracking refs,
//     and a failure is reported as a per-repo error rather than a silent
//     difference.
//   - GHORG_FETCH_PRUNE prunes stale remote-tracking refs on that fetch.
//   - GHORG_GIT_FILTER makes a partial clone (git/git.go:106-109). The checkout
//     still materialises HEAD in full — git fetches the filtered blobs it needs —
//     so what lands is a complete tree backed by a promisor .git.
//
// Two knobs that would otherwise belong here are handled more strongly instead:
// GHORG_TARGET_REPOS_PATH and GHORG_CLONE_TYPE are pinned as flags in buildArgs,
// and a ghorg flag outranks both the environment and the config file.
//
// ambientGhorgEnv lists GHORG_* environment variables that would let host config
// silently change which repos end up in the workspace. goldfinger's guarantee is
// that the workspace holds the lockfile set, so every way to break that belongs
// here, and there are four:
//
//   - narrowing it — topics/prefix/regex/language/archived/forks/user-option, or
//     a pointed-at ghorgignore;
//   - widening it — GHORG_CLONE_WIKI and GHORG_GITHUB_USER_GISTS add clone
//     targets goldfinger never selected (the target-repos filter deliberately
//     lets a selected repo's `.wiki` through, and the gists path returns before
//     normal repo discovery runs at all);
//   - emptying it — GHORG_DRY_RUN makes ghorg return without cloning, so a real
//     mirror silently produces nothing;
//   - deleting from it — the prune knobs remove clones already on disk.
//
// So these are scrubbed from ghorg's environment: the lockfile, not the host,
// decides what gets mirrored.
//
// The scrub is one of two halves and cannot stand alone: ghorg re-creates any of
// these from its config file, which a missing environment variable does not
// suppress. The empty --config pinned in buildArgs is what closes that half.
var ambientGhorgEnv = []string{
	"GHORG_TOPICS",
	"GHORG_MATCH_PREFIX",
	"GHORG_EXCLUDE_MATCH_PREFIX",
	"GHORG_MATCH_REGEX",
	"GHORG_EXCLUDE_MATCH_REGEX",
	"GHORG_GITHUB_FILTER_LANGUAGE",
	"GHORG_SKIP_ARCHIVED",
	"GHORG_SKIP_FORKS",
	// Which of a user's repos ghorg asks GitHub for (owner/member/all) when the
	// selection's owner is a user: it becomes the `type` on the list call
	// (scm/github.go:90-96), so an ambient `member` drops every repo the user owns
	// — a lockfile entry that simply never appears in the listing.
	"GHORG_GITHUB_USER_OPTION",
	"GHORG_IGNORE_PATH",
	// The ghorgonly allowlist's path. Pinned in argv too (an absent path, which is
	// how the allowlist is switched off) — the flag is what actually disarms it,
	// since with the variable merely stripped ghorg falls back to the host's
	// default ghorgonly rather than to no allowlist at all.
	"GHORG_ONLY_PATH",
	"GHORG_CLONE_WIKI",
	"GHORG_GITHUB_USER_GISTS",
	"GHORG_DRY_RUN",
	"GHORG_PRUNE",
	"GHORG_PRUNE_NO_CONFIRM",
	"GHORG_PRUNE_UNTOUCHED",
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
// them either — the empty --config now shuts that file off wholesale, and these
// stay pinned so the layout holds by explicit argv rather than by the config
// being empty. GHORG_PRESERVE_DIRECTORY_STRUCTURE is GitLab-only, so scrubbing
// its env is enough (goldfinger only clones GitHub).
var layoutGhorgEnv = []string{
	// The clone root. Pinned in argv as --path on every real run, but scrubbed too
	// so the layout guarantee holds at this package's boundary rather than
	// depending on every caller filling in Options.Workspace.
	"GHORG_ABSOLUTE_PATH_TO_CLONE_TO",
	"GHORG_OUTPUT_DIR",
	"GHORG_PRESERVE_SCM_HOSTNAME",
	"GHORG_PRESERVE_DIRECTORY_STRUCTURE",
	// Backup mode moves the clones to <owner>_backup AND makes them bare mirrors
	// (cmd/clone.go:1520, git/git.go:112) — so the path goldfinger printed holds
	// nothing, and what it does hold has no working tree for `scan` to read.
	"GHORG_BACKUP",
}

// contentGhorgEnv lists GHORG_* environment variables that change what is INSIDE
// each clone. They are the mirror-side counterpart to the digest keys apply
// occupies in its multi-gitter config: goldfinger models each of them as an
// option of its own (Options.Branch, Options.CloneDepth, Options.NoClean) and
// passes the ghorg flag ONLY when the operator asked for it, which is exactly the
// shape a host default fills in unnoticed.
//
// GHORG_CLONE_DEPTH is the one that bites hardest, because it silently
// reconstitutes a combination goldfinger refuses on the command line: a shallow
// clone only fetches each repo's default branch, so `mirror --branch dev` plus an
// ambient depth quietly leaves every dev-isn't-default repo on the wrong branch —
// and branch presence is a selection-time fact, so no report contradicts it.
// GHORG_BRANCH does the same thing from the other end, checking out a host-chosen
// branch when the operator named none; GHORG_NO_CLEAN leaves local modifications
// in place in clones a later `scan` will read as the repo's contents, and
// GHORG_PROTECT_LOCAL is the harder version of that — it makes ghorg skip an
// existing clone with local changes outright (cmd/repository_processor.go:263-273),
// so the stale tree survives while the directory's mere existence still satisfies
// the reconciliation count.
//
// GHORG_INCLUDE_SUBMODULES is the one that reaches furthest, because it is the
// only knob here that can put a repo goldfinger never selected inside the
// workspace: it adds --recursive to the clone and --recurse-submodules to the
// pull (git/git.go:94-97, git/git.go:226-229), and a submodule's files are
// ordinary files in an ordinary subdirectory. scan skips .git but reads
// everything else under a repo root and attributes it to that root, so a
// submodule's contents would be reported as matches in a lockfile repo — the
// same-set guarantee broken from inside the clone rather than at selection.
var contentGhorgEnv = []string{
	"GHORG_BRANCH",
	"GHORG_CLONE_DEPTH",
	"GHORG_NO_CLEAN",
	"GHORG_PROTECT_LOCAL",
	"GHORG_INCLUDE_SUBMODULES",
}

// authRouteGhorgEnv lists GHORG_* environment variables that change WHERE the
// mapped token ends up: which host ghorg sends it to, which credential the clone
// presents instead, and — the case that is easy to miss because it looks like a
// verbosity knob rather than an auth one — whether ghorg writes the token down.
// They are the mirror-side counterpart to the identity keys apply occupies in its
// multi-gitter config (token, username, base-url, platform, ssh-auth): scrubbing
// the credential vars settles which token EXISTS, these settle who ends up using
// it and where. Without them the identity claim is only half kept — goldfinger
// would hand ghorg the right token and let the host decide who to send it to.
//
//   - GHORG_SCM_BASE_URL repoints ghorg's API calls at an arbitrary host, and
//     those calls carry GHORG_GITHUB_TOKEN — one host variable turns a mirror run
//     into handing the operator's PAT to someone else's server. Verified against
//     v1.11.14: with an empty --config the run reaches
//     https://evil.example/api/v3/users/<owner>/repos.
//   - GHORG_CLONE_PROTOCOL=ssh swaps the token-bearing HTTPS clone URL for
//     git@github.com (scm/github.go:249-254), so the clone authenticates with
//     whatever key the operator's ssh-agent holds instead of the identity
//     goldfinger resolved and announced — and a repo the PAT cannot see may clone
//     anyway, or one it can may not.
//   - GHORG_SSH_HOSTNAME rewrites the host inside that SSH URL, aiming the clone
//     at a machine of the config author's choosing.
//   - GHORG_SCM_TYPE routes the whole host run at a different forge, where the
//     GitHub-resolved names in the lockfile mean something else entirely.
//   - GHORG_DEBUG makes ghorg PRINT the token, by three separate routes:
//     getOrSetDefaults echoes every resolved variable including
//     GHORG_GITHUB_TOKEN (cmd/root.go:271-273, :358), viper.Debug() dumps the
//     whole resolved settings map (cmd/root.go:395), and printDebugCmd
//     spew.Dumps both the repo struct and the exec.Cmd (git/git.go:43-50) whose
//     clone URL has the PAT embedded in it by addTokenToHTTPSCloneURL
//     (scm/github.go:188-190, :249-250). goldfinger streams a delegate's output
//     to stderr without redacting it (cmd/exec.go), so this is the one knob that
//     turns an ordinary mirror run into a credential disclosure — while looking,
//     from the outside, exactly like the colour and quiet knobs deliberately left
//     to the host. That is why the category is about where the token GOES, not
//     merely which host it is sent to: stderr and a scrollback buffer are a
//     destination too. GHORG_CONCURRENCY_DEBUG is not here — it is only read
//     inside the GHORG_DEBUG block (cmd/root.go:307-311) and prints nothing on
//     its own.
//
// The first three are also pinned in argv (--base-url=, --protocol=https,
// --scm=github); see buildArgs. GHORG_SSH_HOSTNAME needs no pin because the
// protocol pin makes ghorg's only two reads of it unreachable, and the scrub is
// then the second lock rather than the first. GHORG_DEBUG has no flag to pin —
// ghorg exposes it as an environment variable only — so for that one the scrub
// and the empty --config are the whole defence.
//
// GHORG_NO_TOKEN is deliberately absent: it only short-circuits ghorg's
// token-is-present check (configs.VerifyTokenSet), and goldfinger always supplies
// the token, so it cannot change which credential is used.
var authRouteGhorgEnv = []string{
	"GHORG_SCM_TYPE",
	"GHORG_SCM_BASE_URL",
	"GHORG_CLONE_PROTOCOL",
	"GHORG_SSH_HOSTNAME",
	"GHORG_DEBUG",
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
	// Map the PAT onto ghorg's own token var, and strip every GHORG_* var that
	// could make the run disagree with what goldfinger resolved and printed: the
	// credential set, then the four categories below — which repos land
	// (ambient), where they land (layout), what is inside them (content), and
	// where the token is spent (authRoute). With the empty --config above — the
	// other half, covering the same names by a route this scrub cannot reach — no
	// credential but the mapped one reaches ghorg, it can only be spent against
	// GitHub over HTTPS, and the host can change neither the set nor the layout
	// out from under the lockfile.
	//
	// Then git's own variables, which belong to none of those categories because
	// they are not ghorg's: ghorg execs `git` and never sets cmd.Env, so git
	// inherits this environment with the PAT sitting in the clone URL ghorg just
	// built for it. See models.GitGrandchildEnvVars — that scrub is shared with
	// apply precisely because the exposure is a property of git, not of either
	// delegate, and so cannot live in a GHORG_* category.
	drop := append(models.CredentialEnvVars(), ambientGhorgEnv...)
	drop = append(drop, layoutGhorgEnv...)
	drop = append(drop, contentGhorgEnv...)
	drop = append(drop, authRouteGhorgEnv...)
	drop = append(drop, models.GitGrandchildEnvVars()...)
	env := overrideEnv(os.Environ(), tokenEnv, token, drop...)
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
		// Pin the on-disk layout to <workspace>/<owner>/<repo> (see layoutGhorgEnv).
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
		// presented to (see authRouteGhorgEnv). ghorg re-Setenv's each of these from
		// argv inside cloneFunc, after its viper/env resolution has run, so a flag
		// beats both the host environment and the host config file. --base-url= is
		// an explicit clear, not an omission: passing it empty is what marks the
		// flag changed, and ghorg then falls back to its own github.com default.
		"--scm=github",
		"--base-url=",
		"--protocol=https",
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
