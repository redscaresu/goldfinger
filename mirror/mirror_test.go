package mirror

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/redscaresu/goldfinger/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func userSelection() models.Selection {
	return models.Selection{
		Version:   models.SelectionVersion,
		Owner:     "redscaresu",
		OwnerType: models.OwnerUser,
		Repos: []models.Repo{
			{Owner: "redscaresu", Name: "goldfinger"},
			{Owner: "redscaresu", Name: "simpleAPI"},
		},
	}
}

// capture records the last invocation the Runner received.
type capture struct {
	name string
	args []string
	env  []string
}

func (c *capture) run(_ context.Context, name string, args, env []string) error {
	c.name, c.args, c.env = name, args, env
	return nil
}

func TestMirrorInvocation(t *testing.T) {
	securityTest(t) // locks the no-token-in-argv invariant (see final assertions)
	var cap capture
	err := Mirror(context.Background(), cap.run, userSelection(), "secret-token",
		Options{Workspace: "/tmp/ws", Concurrency: 20, CloneDepth: 1})
	require.NoError(t, err)

	assert.Equal(t, "ghorg", cap.name)
	assert.Equal(t, "clone", cap.args[0])
	assert.Equal(t, "redscaresu", cap.args[1])
	assert.Contains(t, cap.args, "--clone-type=user")
	assert.Contains(t, cap.args, "--path=/tmp/ws")
	assert.Contains(t, cap.args, "--concurrency=20")
	assert.Contains(t, cap.args, "--clone-depth=1")

	// The token must travel in the env, never argv.
	assert.NotContains(t, strings.Join(cap.args, " "), "secret-token")
	assert.Contains(t, cap.env, tokenEnv+"=secret-token")

	// --target-repos-path points at a file listing the selected repo names.
	var namesPath string
	for _, a := range cap.args {
		if strings.HasPrefix(a, "--target-repos-path=") {
			namesPath = strings.TrimPrefix(a, "--target-repos-path=")
		}
	}
	require.NotEmpty(t, namesPath)
	// File is cleaned up after Mirror returns.
	_, statErr := os.Stat(namesPath)
	assert.True(t, os.IsNotExist(statErr), "names file should be removed after Mirror returns")
}

func TestMirrorWritesRepoNames(t *testing.T) {
	var gotNames string
	run := func(_ context.Context, _ string, args, _ []string) error {
		for _, a := range args {
			if strings.HasPrefix(a, "--target-repos-path=") {
				data, err := os.ReadFile(strings.TrimPrefix(a, "--target-repos-path="))
				require.NoError(t, err)
				gotNames = string(data)
			}
		}
		return nil
	}
	require.NoError(t, Mirror(context.Background(), run, userSelection(), "tok", Options{}))
	assert.Equal(t, "goldfinger\nsimpleAPI\n", gotNames)
}

func TestMirrorOverridesExistingToken(t *testing.T) {
	securityTest(t)
	t.Setenv(tokenEnv, "runner-default-token")
	var cap capture
	require.NoError(t, Mirror(context.Background(), cap.run, userSelection(), "our-token", Options{}))

	var vals []string
	for _, e := range cap.env {
		if strings.HasPrefix(e, tokenEnv+"=") {
			vals = append(vals, e)
		}
	}
	require.Len(t, vals, 1, "exactly one token entry should reach the child")
	assert.Equal(t, tokenEnv+"=our-token", vals[0])
}

// setHostile plants a hostile value for every name the caller lists, as if the
// operator's shell or ghorg config had set it, and hands the names back so the
// assertion side can be driven from the same literal.
func setHostile(t *testing.T, hostile map[string]string) map[string]string {
	t.Helper()
	for name, val := range hostile {
		t.Setenv(name, val)
	}
	return hostile
}

// assertScrubbed proves a scrub category is closed, in BOTH directions, and
// exists because the obvious version of this check is a tautology. Building the
// assertions by ranging over the package list makes the test self-referential:
// deleting a name from the list deletes its own assertion, so the scrub silently
// stops covering it and every test still passes. That is not hypothetical —
// negative-testing this file by removing two entries from contentGhorgEnv
// produced zero failures, which is how the shape was found.
//
// So the names come from the caller's literal map, which fails if a name is
// dropped from the list, and the list must be a SUBSET of that map, which fails
// if a name is added to the list without a hostile value being planted here (an
// assertion against a variable the environment never held passes for free).
// Together the list and the test can only change in step.
func assertScrubbed(t *testing.T, category string, hostile map[string]string, list, env []string) {
	t.Helper()
	names := make([]string, 0, len(hostile))
	for name := range hostile {
		names = append(names, name)
	}
	assert.Subsetf(t, names, list,
		"every %s var must be given a hostile value in this test — one that is not set here is asserted against an environment that never held it", category)
	for _, name := range names {
		for _, e := range env {
			assert.NotContainsf(t, e, name+"=", "%s ghorg var %s must be stripped from the child environment", category, name)
		}
	}
}

func TestMirrorNeutralisesAmbientConfig(t *testing.T) {
	securityTest(t)
	// Ambient GHORG_* vars that would change which repos end up in the workspace
	// must not reach ghorg, and an empty ghorgignore must be forced so no host
	// ignore file can drop repos. All four ways the workspace can come to disagree
	// with the lockfile are represented: narrowed, widened, emptied, pruned.
	hostile := setHostile(t, map[string]string{
		"GHORG_TOPICS":                     "should-be-stripped",
		"GHORG_MATCH_PREFIX":               "keep-",
		"GHORG_EXCLUDE_MATCH_PREFIX":       "drop-",
		"GHORG_MATCH_REGEX":                "^keep-",
		"GHORG_EXCLUDE_MATCH_REGEX":        "^drop-",
		"GHORG_GITHUB_FILTER_LANGUAGE":     "go",
		"GHORG_SKIP_ARCHIVED":              "true",
		"GHORG_SKIP_FORKS":                 "true",
		"GHORG_GITHUB_USER_OPTION":         "member",
		"GHORG_IGNORE_PATH":                "/host/ghorgignore",
		"GHORG_ONLY_PATH":                  "/host/ghorgonly",
		"GHORG_CLONE_WIKI":                 "true",
		"GHORG_GITHUB_USER_GISTS":          "true",
		"GHORG_DRY_RUN":                    "true",
		"GHORG_PRUNE":                      "true",
		"GHORG_PRUNE_NO_CONFIRM":           "true",
		"GHORG_PRUNE_UNTOUCHED":            "true",
		"GHORG_PRUNE_UNTOUCHED_NO_CONFIRM": "true",
	})

	var cap capture
	require.NoError(t, Mirror(context.Background(), cap.run, userSelection(), "tok", Options{}))

	assertScrubbed(t, "ambient", hostile, ambientGhorgEnv, cap.env)

	var ignorePath string
	for _, a := range cap.args {
		if strings.HasPrefix(a, "--ghorgignore-path=") {
			ignorePath = strings.TrimPrefix(a, "--ghorgignore-path=")
		}
	}
	require.NotEmpty(t, ignorePath, "mirror must force an explicit --ghorgignore-path")
	assert.NotEqual(t, "/host/ghorgignore", ignorePath, "must not use the host ghorgignore")
	// File is cleaned up after Mirror returns.
	_, statErr := os.Stat(ignorePath)
	assert.True(t, os.IsNotExist(statErr), "temp ghorgignore should be removed after Mirror returns")
}

// TestMirrorPinsEmptyGhorgConfig locks the half of the ambient-config defence the
// environment scrub cannot cover. ghorg resolves every GHORG_* knob through viper
// and os.Setenv's the result into its own environment, so its config file does
// not merely survive a stripped variable — it RE-CREATES it. Unpinned, a host
// ~/.config/ghorg/conf.yaml (or a stray ./ghorg.yaml in whatever directory
// goldfinger was run from) puts back GHORG_TOPICS and friends, narrowing the set
// below the lockfile, and puts back GHORG_GITHUB_APP_PEM_PATH, which makes ghorg
// authenticate as a GitHub App instead of the PAT goldfinger resolved and
// announced. Only a flag outranks the file, so the assertion is that one is
// always passed and that it points at a file that can say nothing.
func TestMirrorPinsEmptyGhorgConfig(t *testing.T) {
	securityTest(t)
	t.Setenv("GHORG_CONFIG", "/host/conf.yaml")

	var cap capture
	var duringRun struct {
		size int64
		err  error
	}
	run := func(ctx context.Context, name string, args, env []string) error {
		// Stat from inside the run: Mirror removes the file on return, and "empty"
		// is only meaningful while ghorg could still read it.
		for _, a := range args {
			if path, ok := strings.CutPrefix(a, "--config="); ok {
				var info os.FileInfo
				info, duringRun.err = os.Stat(path)
				if duringRun.err == nil {
					duringRun.size = info.Size()
				}
			}
		}
		return cap.run(ctx, name, args, env)
	}
	require.NoError(t, Mirror(context.Background(), run, userSelection(), "tok", Options{}))

	var configPaths []string
	for _, a := range cap.args {
		if path, ok := strings.CutPrefix(a, "--config="); ok {
			configPaths = append(configPaths, path)
		}
	}
	require.Len(t, configPaths, 1, "mirror must pass exactly one --config; a second would let the last one win by accident")
	configPath := configPaths[0]
	assert.NotEqual(t, "/host/conf.yaml", configPath, "must not hand ghorg the host config")

	require.NoError(t, duringRun.err, "the pinned config must exist while ghorg runs")
	assert.Zero(t, duringRun.size, "the pinned config must be empty — a non-empty one could set the very knobs it exists to suppress")

	// The extension is load-bearing, not cosmetic: ghorg passes the path to
	// viper.SetConfigFile, which picks its parser from the extension and exits(1)
	// on one it does not recognise. An empty file with no suffix would abort every
	// mirror run.
	assert.Equal(t, ".yaml", filepath.Ext(configPath), "the pinned config must be parseable by ghorg's config loader")

	_, statErr := os.Stat(configPath)
	assert.True(t, os.IsNotExist(statErr), "temp ghorg config should be removed after Mirror returns")
}

// TestMirrorDisarmsGhorgonly guards a filter that fails in the opposite direction
// to the ghorgignore beside it. ghorgonly is an ALLOWLIST that ghorg reads from
// $HOME/.config/ghorg/ghorgonly by default, so an operator who happens to have one
// would silently mirror less than the lockfile — and the empty-file trick that
// disarms ghorgignore would be catastrophic here, matching no repo and cloning
// nothing at all. The off switch is a path that does not exist, which is what this
// asserts: the assertion that the file is ABSENT is the whole point, not an
// oversight.
func TestMirrorDisarmsGhorgonly(t *testing.T) {
	securityTest(t)
	t.Setenv("GHORG_ONLY_PATH", "/host/ghorgonly")

	var cap capture
	var existedDuringRun bool
	run := func(ctx context.Context, name string, args, env []string) error {
		for _, a := range args {
			if path, ok := strings.CutPrefix(a, "--ghorgonly-path="); ok {
				_, err := os.Stat(path)
				existedDuringRun = err == nil
			}
		}
		return cap.run(ctx, name, args, env)
	}
	require.NoError(t, Mirror(context.Background(), run, userSelection(), "tok", Options{}))

	var onlyPaths []string
	for _, a := range cap.args {
		if path, ok := strings.CutPrefix(a, "--ghorgonly-path="); ok {
			onlyPaths = append(onlyPaths, path)
		}
	}
	require.Len(t, onlyPaths, 1, "mirror must force exactly one --ghorgonly-path")
	assert.NotEqual(t, "/host/ghorgonly", onlyPaths[0], "must not use the host ghorgonly")
	assert.False(t, existedDuringRun,
		"the pinned ghorgonly must NOT exist: ghorg skips the allowlist only when the path is absent, and an empty file there would match no repo and clone nothing")
}

func TestMirrorStripsSourcePATFromChildEnv(t *testing.T) {
	securityTest(t)
	t.Setenv(models.TokenEnvVar, "raw-pat") // operator's exported GOLD_FINGER_PAT
	var cap capture
	require.NoError(t, Mirror(context.Background(), cap.run, userSelection(), "mapped-token", Options{}))

	for _, e := range cap.env {
		assert.NotContains(t, e, models.TokenEnvVar+"=", "source PAT var must not reach the child")
		assert.NotContains(t, e, "raw-pat", "raw PAT value must not reach the child under any name")
	}
	assert.Contains(t, cap.env, tokenEnv+"=mapped-token")
}

// TestMirrorScrubsEveryCredentialVarFromChildEnv locks the identity invariant for
// the mirror path: ghorg needs exactly one credential — its own — and every other
// credential-bearing variable the host happens to export is stripped, so no
// ambient token can decide which account clones the lockfile set. Asserting on the
// whole credential set (not just the source PAT) is the point: a
// GOLD_FINGER_PAT-only test passed for the entire time this gap was open.
func TestMirrorScrubsEveryCredentialVarFromChildEnv(t *testing.T) {
	securityTest(t)
	for _, v := range models.CredentialEnvVars() {
		t.Setenv(v, "ambient-"+v)
	}
	t.Setenv("GOLDFINGER_TEST_UNRELATED", "keep-me")

	var cap capture
	require.NoError(t, Mirror(context.Background(), cap.run, userSelection(), "mapped-token", Options{}))

	assert.Equal(t, map[string]string{tokenEnv: "mapped-token"}, credentialVarsIn(cap.env),
		"exactly one credential variable — ghorg's own, carrying goldfinger's resolved token — may reach the child")
	for _, e := range cap.env {
		assert.NotContains(t, e, "ambient-", "no ambient credential value may reach the child under any name")
	}
	assert.Contains(t, cap.env, "GOLDFINGER_TEST_UNRELATED=keep-me", "the scrub must not strip unrelated vars")
}

// TestMirrorScrubsGitGrandchildVarsFromChildEnv covers the two routes that go
// around every control in this file. ghorg execs `git` for each clone and never
// sets cmd.Env (git/git.go, 19 exec sites, zero Env assignments), and because
// goldfinger pins --protocol=https ghorg takes addTokenToHTTPSCloneURL
// (scm/github.go:188), so the live PAT is in git's argv. From there GIT_TRACE
// writes that argv to a file the host chose, and GIT_CONFIG_COUNT with an
// insteadOf rewrite sends the remote — token included — to a host goldfinger never
// resolved. Both happen once per repo, while every GHORG_* scrub above passes.
func TestMirrorScrubsGitGrandchildVarsFromChildEnv(t *testing.T) {
	securityTest(t)
	setGitHostileEnv(t)

	var cap capture
	require.NoError(t, Mirror(context.Background(), cap.run, userSelection(), "mapped-token", Options{}))

	assertGitEnvScrubbed(t, cap.env)
}

// TestOverrideEnvPrefixDropStopsAtTheFamily pins the mechanism rather than the
// list. A prefix entry is the only kind of drop that removes variables nobody
// enumerated, which makes its boundary the thing worth testing: it must strip the
// whole family including members added after this was written, and nothing that
// merely starts with the same letters. Not duplicated into apply/ because
// TestDuplicatedDeclsHaveNotDrifted proves that copy of overrideEnv is identical.
func TestOverrideEnvPrefixDropStopsAtTheFamily(t *testing.T) {
	securityTest(t)
	base := []string{
		"GIT_TRACE=/tmp/x",
		"GIT_TRACE2_EVENT=/tmp/x",
		"GIT_TRACE_SOMETHING_INVENTED_LATER=1",
		"GIT_CONFIG_KEY_0=url.https://evil/.insteadOf",
		"GIT_CONFIG_KEY_37=url.https://evil/.insteadOf",
		"GIT_CONFIG_GLOBAL=/home/op/.gitconfig",
		"GIT_TERMINAL_PROMPT=0",
		"GITHUB_ACTIONS=true",
		"GIT=notavar",
	}
	got := overrideEnv(base, "TOKEN", "t", "GIT_TRACE*", "GIT_CONFIG_KEY_*")

	assert.ElementsMatch(t, []string{
		"GIT_CONFIG_GLOBAL=/home/op/.gitconfig",
		"GIT_TERMINAL_PROMPT=0",
		"GITHUB_ACTIONS=true",
		"GIT=notavar",
		"TOKEN=t",
	}, got, "a prefix drop must take the whole family — including the unbounded GIT_CONFIG_KEY_n and names not yet invented — and stop at the prefix, leaving GIT_CONFIG_GLOBAL alone")
}

// credentialVarsIn returns the credential-bearing entries of a child environment,
// as name→value.
func credentialVarsIn(env []string) map[string]string {
	creds := map[string]string{}
	for _, e := range env {
		name, val, _ := strings.Cut(e, "=")
		for _, v := range models.CredentialEnvVars() {
			if name == v {
				creds[name] = val
			}
		}
	}
	return creds
}

func TestMirrorPinsLayoutAgainstHostConfig(t *testing.T) {
	securityTest(t)
	// The layout <workspace>/<owner>/<repo> is what goldfinger prints, reports, and
	// reconciles against, so every ghorg knob that could move clones must be both
	// pinned in argv (a CLI flag overrides env AND config) and scrubbed from the
	// child env. Setting all of them here must not change the resulting layout.
	// GHORG_BACKUP is the sharpest of these: it both relocates the clones to
	// <owner>_backup and makes them bare, so the path goldfinger printed would be
	// empty and nothing on disk would have a working tree for `scan` to read.
	hostile := setHostile(t, map[string]string{
		"GHORG_ABSOLUTE_PATH_TO_CLONE_TO":    "/host/clones",
		"GHORG_OUTPUT_DIR":                   "host-output",
		"GHORG_PRESERVE_SCM_HOSTNAME":        "true",
		"GHORG_PRESERVE_DIRECTORY_STRUCTURE": "true",
		"GHORG_BACKUP":                       "true",
	})

	var cap capture
	require.NoError(t, Mirror(context.Background(), cap.run, userSelection(), "tok", Options{}))

	assert.Contains(t, cap.args, "--output-dir=redscaresu",
		"output dir must be pinned to the owner so GHORG_OUTPUT_DIR can't relocate clones")
	assert.Contains(t, cap.args, "--preserve-scm-hostname=false",
		"scm-hostname nesting must be pinned off so clones stay directly under <ws>/<owner>")
	assertScrubbed(t, "layout", hostile, layoutGhorgEnv, cap.env)
}

// TestMirrorNeutralisesAmbientCloneContent covers the category goldfinger is most
// likely to leave open, because these are knobs it models itself: Branch,
// CloneDepth and NoClean are goldfinger Options whose ghorg flag is passed ONLY
// when the operator asked for one, so a host default lands in exactly the gap.
//
// The combination asserted here is the one goldfinger refuses on its own command
// line: --branch with a shallow clone. A shallow clone fetches only each repo's
// default branch, so an ambient GHORG_CLONE_DEPTH would leave every repo where
// the branch is not the default checked out at the wrong revision — and branch
// presence is a selection-time fact, so no report would contradict it.
//
// GHORG_INCLUDE_SUBMODULES is the widest of them: it is the only knob in any of
// these lists that can land a repo the lockfile never named inside the workspace,
// because a submodule's files are ordinary files in an ordinary subdirectory and
// scan attributes everything under a repo root to that root.
func TestMirrorNeutralisesAmbientCloneContent(t *testing.T) {
	securityTest(t)
	hostile := setHostile(t, map[string]string{
		"GHORG_BRANCH":             "host-branch",
		"GHORG_CLONE_DEPTH":        "1",
		"GHORG_NO_CLEAN":           "true",
		"GHORG_PROTECT_LOCAL":      "true",
		"GHORG_INCLUDE_SUBMODULES": "true",
	})

	var cap capture
	require.NoError(t, Mirror(context.Background(), cap.run, userSelection(), "tok", Options{Branch: "dev"}))

	assertScrubbed(t, "clone-content", hostile, contentGhorgEnv, cap.env)
	assert.Contains(t, cap.args, "--branch=dev", "the operator's branch must still be the one passed")
	for _, a := range cap.args {
		assert.NotContains(t, a, "--clone-depth", "no depth was asked for, so none may be applied")
	}
}

// TestMirrorPinsAuthRouteAgainstHostConfig locks the other half of the identity
// invariant. Scrubbing the credential vars decides which token exists; this
// decides who spends it and where. The two knobs that matter fail in opposite
// directions and both are silent: GHORG_SCM_BASE_URL sends the PAT itself to a
// host of the config author's choosing, and GHORG_CLONE_PROTOCOL=ssh leaves the
// PAT unused and clones with the operator's ssh key instead — so goldfinger would
// announce one principal while ghorg acted as another, which is exactly what the
// credential scrub exists to prevent.
//
// Setting all four here must change nothing about the resulting invocation.
func TestMirrorPinsAuthRouteAgainstHostConfig(t *testing.T) {
	securityTest(t)
	hostile := setHostile(t, map[string]string{
		"GHORG_SCM_BASE_URL":   "https://evil.example/api/v3",
		"GHORG_CLONE_PROTOCOL": "ssh",
		"GHORG_SSH_HOSTNAME":   "evil-alias",
		"GHORG_SCM_TYPE":       "gitlab",
		// The odd one out, and the reason this category is "where the token goes"
		// rather than "which host it is sent to": GHORG_DEBUG makes ghorg print the
		// PAT — as a resolved env value, in viper's settings dump, and inside the
		// clone URL spew.Dump'd with the git command — and goldfinger streams a
		// delegate's output to stderr unredacted.
		"GHORG_DEBUG": "1",
	})

	var cap capture
	require.NoError(t, Mirror(context.Background(), cap.run, userSelection(), "tok", Options{}))

	assert.Contains(t, cap.args, "--protocol=https",
		"the protocol must be pinned to https so the clone presents the mapped token, not the operator's ssh key")
	assert.Contains(t, cap.args, "--base-url=",
		"the base URL must be explicitly cleared so the token can only be spent against github.com")
	assert.Contains(t, cap.args, "--scm=github",
		"the forge must be pinned so the lockfile's GitHub names are resolved against GitHub")
	assertScrubbed(t, "auth-route", hostile, authRouteGhorgEnv, cap.env)
}

func TestMirrorOrgCloneType(t *testing.T) {
	s := userSelection()
	s.OwnerType = models.OwnerOrganization
	var cap capture
	require.NoError(t, Mirror(context.Background(), cap.run, s, "tok", Options{}))
	assert.Contains(t, cap.args, "--clone-type=org")
}

func TestMirrorDryRun(t *testing.T) {
	var cap capture
	require.NoError(t, Mirror(context.Background(), cap.run, userSelection(), "tok", Options{DryRun: true}))
	assert.Contains(t, cap.args, "--dry-run")
}

func TestMirrorNoClean(t *testing.T) {
	var cap capture
	require.NoError(t, Mirror(context.Background(), cap.run, userSelection(), "tok", Options{}))
	assert.NotContains(t, cap.args, "--no-clean", "omitted by default")

	cap = capture{}
	require.NoError(t, Mirror(context.Background(), cap.run, userSelection(), "tok", Options{NoClean: true}))
	assert.Contains(t, cap.args, "--no-clean")
}

func TestMirrorBranch(t *testing.T) {
	var cap capture
	require.NoError(t, Mirror(context.Background(), cap.run, userSelection(), "tok", Options{}))
	for _, a := range cap.args {
		assert.NotContains(t, a, "--branch", "omitted by default so ghorg uses each repo's default")
	}

	cap = capture{}
	require.NoError(t, Mirror(context.Background(), cap.run, userSelection(), "tok", Options{Branch: "dev"}))
	assert.Contains(t, cap.args, "--branch=dev")
}

func TestMirrorEmptySelection(t *testing.T) {
	err := Mirror(context.Background(), func(context.Context, string, []string, []string) error {
		t.Fatal("runner should not be called for an empty selection")
		return nil
	}, models.Selection{Owner: "x"}, "tok", Options{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}
