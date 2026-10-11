package apply

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redscaresu/goldfinger/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func twoRepoSelection() models.Selection {
	return models.Selection{
		Owner:     "redscaresu",
		OwnerType: models.OwnerUser,
		Repos: []models.Repo{
			{Owner: "redscaresu", Name: "shellspy"},
			{Owner: "redscaresu", Name: "reverseastring"},
		},
	}
}

func baseSpec() models.ApplySpec {
	return models.ApplySpec{
		Branch:        "bump",
		CommitMessage: "bump image",
		PRTitle:       "Bump image",
		Script:        []string{"sed", "-i", "s|a|b|", "Dockerfile"},
		DryRun:        true,
		// Apply now guards that every run names a signing mode; SignNone adds no
		// multi-gitter flag, so it keeps the arg assertions below unchanged.
		Sign: models.SignNone,
	}
}

type capture struct {
	name string
	args []string
	env  []string
}

func (c *capture) run(_ context.Context, name string, args, env []string) ([]byte, error) {
	c.name, c.args, c.env = name, args, env
	return nil, nil
}

func TestApplyInvocation(t *testing.T) {
	securityTest(t) // locks the no-token-in-argv invariant (see final assertions)
	var cap capture
	spec := baseSpec()
	spec.PRBody = "details"
	spec.Labels = []string{"fleet", "chore"}
	spec.Reviewers = []string{"redscaresu"}
	spec.Draft = true
	spec.BaseBranch = "dev"

	_, err := Apply(context.Background(), cap.run, twoRepoSelection(), spec, "secret-token")
	require.NoError(t, err)

	assert.Contains(t, cap.args, "--base-branch=dev")

	assert.Equal(t, "multi-gitter", cap.name)
	assert.Equal(t, "run", cap.args[0])

	joined := strings.Join(cap.args, " ")
	assert.Contains(t, joined, "--repo=redscaresu/shellspy")
	assert.Contains(t, joined, "--repo=redscaresu/reverseastring")
	assert.Contains(t, cap.args, "--branch=bump")
	assert.Contains(t, cap.args, "--commit-message=bump image")
	assert.Contains(t, cap.args, "--pr-title=Bump image")
	assert.Contains(t, cap.args, "--pr-body=details")
	assert.Contains(t, cap.args, "--labels=fleet")
	assert.Contains(t, cap.args, "--labels=chore")
	assert.Contains(t, cap.args, "--reviewers=redscaresu")
	assert.Contains(t, cap.args, "--draft")
	assert.Contains(t, cap.args, "--dry-run")

	// Token in env, never argv.
	assert.NotContains(t, joined, "secret-token")
	assert.Contains(t, cap.env, tokenEnv+"=secret-token")
}

func TestApplyOneRepoFlagPerRepo(t *testing.T) {
	var cap capture
	_, err := Apply(context.Background(), cap.run, twoRepoSelection(), baseSpec(), "t")
	require.NoError(t, err)
	n := 0
	for _, a := range cap.args {
		if strings.HasPrefix(a, "--repo=") {
			n++
		}
	}
	assert.Equal(t, 2, n)
}

func TestApplyWrapsScript(t *testing.T) {
	var scriptBody string
	var scriptPath string
	run := func(_ context.Context, _ string, args, _ []string) ([]byte, error) {
		scriptPath = args[1] // run <scriptPath>
		data, err := os.ReadFile(scriptPath)
		require.NoError(t, err)
		scriptBody = string(data)
		return nil, nil
	}
	_, err := Apply(context.Background(), run, twoRepoSelection(), baseSpec(), "t")
	require.NoError(t, err)

	assert.Contains(t, scriptBody, "#!/bin/sh")
	assert.Contains(t, scriptBody, "exec 'sed' '-i' 's|a|b|' 'Dockerfile'")
	// Script is cleaned up after Apply returns.
	_, statErr := os.Stat(scriptPath)
	assert.True(t, os.IsNotExist(statErr))
}

func TestApplyNoDryRunOmitsFlag(t *testing.T) {
	var cap capture
	spec := baseSpec()
	spec.DryRun = false
	spec.Confirm = true // a live run must be explicitly confirmed at the boundary
	_, err := Apply(context.Background(), cap.run, twoRepoSelection(), spec, "t")
	require.NoError(t, err)
	assert.NotContains(t, cap.args, "--dry-run")
}

// TestApplyRefusesUnconfirmedLiveRun locks charter invariant (a) at the
// execution boundary: a non-dry-run apply that isn't confirmed must be refused
// by apply.Apply itself — not only by the Cobra --confirm flag — so a caller
// that constructs an ApplySpec directly (e.g. a future MCP adapter) cannot open
// PRs by omitting confirmation. failRunner asserts multi-gitter is never invoked.
func TestApplyRefusesUnconfirmedLiveRun(t *testing.T) {
	securityTest(t)
	spec := baseSpec()
	spec.DryRun = false
	spec.Confirm = false
	_, err := Apply(context.Background(), failRunner(t), twoRepoSelection(), spec, "t")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Confirm")
}

// TestApplyRequiresValidSignMode locks charter invariant (b): every run must
// name a recognised signing mode. The check is unconditional — asserted here on
// a dry run — so apply.Apply can never fall through to unsigned commits for an
// empty/unknown mode, which the bare multi-gitter default would produce.
func TestApplyRequiresValidSignMode(t *testing.T) {
	securityTest(t)
	for _, mode := range []string{"", "bogus"} {
		name := mode
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			spec := baseSpec() // dry-run: proves --sign is required on every run, not just live
			spec.Sign = mode
			_, err := Apply(context.Background(), failRunner(t), twoRepoSelection(), spec, "t")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "signing mode")
		})
	}
}

// TestApplyAllowsConfirmedSignedLiveRun is the positive counterpart: a live run
// that is both confirmed and signed passes the guards and delegates normally.
func TestApplyAllowsConfirmedSignedLiveRun(t *testing.T) {
	var cap capture
	spec := baseSpec()
	spec.DryRun = false
	spec.Confirm = true
	spec.Sign = models.SignLocal
	_, err := Apply(context.Background(), cap.run, twoRepoSelection(), spec, "t")
	require.NoError(t, err)
	assert.Equal(t, "multi-gitter", cap.name)
	assert.Contains(t, cap.args, "--git-type=cmd")
}

// TestApplyRefusesConfirmedLiveRunWithInvalidSign closes the gap the two guards
// leave individually: a run that CLEARS the confirm guard (DryRun=false,
// Confirm=true) but names no valid signing mode must still be refused — by the
// sign guard (b), unconditionally, before writeScript or any exec. The other
// sign-mode test runs on a dry-run; this proves guard (b) is not skipped once a
// live run has satisfied guard (a), so a confirmed-but-unsigned-mode MCP/direct
// caller cannot slip an empty or bogus mode through to multi-gitter's own
// (silently unsigned) default. failRunner asserts multi-gitter is never invoked.
func TestApplyRefusesConfirmedLiveRunWithInvalidSign(t *testing.T) {
	securityTest(t)
	for _, mode := range []string{"", "bogus"} {
		name := mode
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			spec := baseSpec()
			spec.DryRun = false
			spec.Confirm = true // clears guard (a); guard (b) must still fire
			spec.Sign = mode
			_, err := Apply(context.Background(), failRunner(t), twoRepoSelection(), spec, "t")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "signing mode")
		})
	}
}

// TestApplyAllowsConfirmedLiveRunSignNone documents an intentional-by-design
// path so no one later "hardens" it into a refusal: SignNone ("none") is a valid,
// explicit signing mode (unsigned commits), so a confirmed live run with it is
// PERMITTED and delegates normally, adding no signing flag. "Unsigned live" is a
// legitimate operator choice; only an empty/unrecognised mode is refused (above).
func TestApplyAllowsConfirmedLiveRunSignNone(t *testing.T) {
	var cap capture
	spec := baseSpec()
	spec.DryRun = false
	spec.Confirm = true
	spec.Sign = models.SignNone
	_, err := Apply(context.Background(), cap.run, twoRepoSelection(), spec, "t")
	require.NoError(t, err)
	assert.Equal(t, "multi-gitter", cap.name)
	assert.NotContains(t, cap.args, "--git-type=cmd")
	assert.NotContains(t, cap.args, "--api-push")
}

func TestApplyEmptySelection(t *testing.T) {
	_, err := Apply(context.Background(), failRunner(t), models.Selection{Owner: "x"}, baseSpec(), "t")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

func TestApplyTooManyRepos(t *testing.T) {
	s := models.Selection{Owner: "big"}
	for i := 0; i <= maxRepos; i++ {
		s.Repos = append(s.Repos, models.Repo{Owner: "big", Name: "r"})
	}
	_, err := Apply(context.Background(), failRunner(t), s, baseSpec(), "t")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "limit")
}

func TestApplyOverridesExistingToken(t *testing.T) {
	securityTest(t)
	t.Setenv(tokenEnv, "runner-default-token") // e.g. CI's own GITHUB_TOKEN
	var cap capture
	_, err := Apply(context.Background(), cap.run, twoRepoSelection(), baseSpec(), "our-token")
	require.NoError(t, err)

	var vals []string
	for _, e := range cap.env {
		if strings.HasPrefix(e, tokenEnv+"=") {
			vals = append(vals, e)
		}
	}
	require.Len(t, vals, 1, "exactly one token entry should reach the child")
	assert.Equal(t, tokenEnv+"=our-token", vals[0])
}

func TestApplyStripsSourcePATFromChildEnv(t *testing.T) {
	securityTest(t)
	t.Setenv(models.TokenEnvVar, "raw-pat") // operator's exported GOLD_FINGER_PAT
	var cap capture
	_, err := Apply(context.Background(), cap.run, twoRepoSelection(), baseSpec(), "mapped-token")
	require.NoError(t, err)

	for _, e := range cap.env {
		assert.NotContains(t, e, models.TokenEnvVar+"=", "source PAT var must not reach the child")
		assert.NotContains(t, e, "raw-pat", "raw PAT value must not reach the child under any name")
	}
	assert.Contains(t, cap.env, tokenEnv+"=mapped-token")
}

// TestApplyScrubsEveryCredentialVarFromChildEnv locks the identity invariant: the
// account goldfinger announces and the account a child acts as are the same by
// construction. multi-gitter runs the operator's change script as a grandchild of
// this process, and gh resolves GH_TOKEN AHEAD of the GITHUB_TOKEN goldfinger
// sets — so a script that shells out to gh under an ambient GH_TOKEN would open
// PRs as a different account, silently. Asserting on the whole credential set (not
// just the source PAT) is the point: a GOLD_FINGER_PAT-only test passed for the
// entire time this gap was open.
func TestApplyScrubsEveryCredentialVarFromChildEnv(t *testing.T) {
	securityTest(t)
	for _, v := range models.CredentialEnvVars() {
		t.Setenv(v, "ambient-"+v)
	}
	t.Setenv("GOLDFINGER_TEST_UNRELATED", "keep-me")

	var cap capture
	_, err := Apply(context.Background(), cap.run, twoRepoSelection(), baseSpec(), "mapped-token")
	require.NoError(t, err)

	assert.Equal(t, map[string]string{tokenEnv: "mapped-token"}, credentialVarsIn(cap.env),
		"exactly one credential variable — multi-gitter's own, carrying goldfinger's resolved token — may reach the child")
	for _, e := range cap.env {
		assert.NotContains(t, e, "ambient-", "no ambient credential value may reach the child under any name")
	}
	assert.Contains(t, cap.env, "GOLDFINGER_TEST_UNRELATED=keep-me", "the scrub must not strip unrelated vars")
}

// TestApplyScrubsGitGrandchildVarsFromChildEnv is the one surface where the
// mirror and apply sides are SYMMETRIC, which is why it is worth a test on both
// and a comment saying so. Everywhere else, multi-gitter is excused from the
// log-knob occupation that ghorg needs, because it installs a CensorFormatter
// rewriting the token to "<TOKEN>" in every line it logs (cmd/logging.go:69-78).
// That argument stops here: under --git-type=cmd — which is exactly what
// --sign=local selects — multi-gitter execs git without setting cmd.Env
// (internal/git/cmdgit/git.go:66) after embedding the PAT in the clone URL
// (internal/scm/github/repository.go:20-21), and git writes its own trace file.
// multi-gitter never sees those bytes, so it cannot censor them.
func TestApplyScrubsGitGrandchildVarsFromChildEnv(t *testing.T) {
	securityTest(t)
	setGitHostileEnv(t)

	var cap capture
	spec := baseSpec()
	spec.Sign = models.SignLocal // the mode that makes multi-gitter shell out to git
	_, err := Apply(context.Background(), cap.run, twoRepoSelection(), spec, "mapped-token")
	require.NoError(t, err)

	assertGitEnvScrubbed(t, cap.env)
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

// occupiedConfigKeys are the multi-gitter settings goldfinger must claim in the
// config file it writes, with why each one matters. Claiming a key is what stops
// multi-gitter's static ~/.multi-gitter/config filling it in, because a config
// value is only applied to a flag that is not already set — so a key dropped from
// this list silently hands that setting back to the host.
//
// It is spelled out rather than derived from multiGitterNeutralConfig, which
// would assert nothing: the point is that removing a key has to be a deliberate,
// reviewed act, and the reason has to survive with it.
//
// Each entry carries the VALUE as well as the key, and that half matters at least
// as much. Occupancy alone is not the guarantee — the guarantee is occupancy with
// the flag's neutral default, because goldfinger's --config is layered ABOVE the
// host's static file and applied to any flag the argv did not set, so whatever
// value sits here is authoritative. Asserting only that a key is present would
// pass a config that read `skip-pr: true`, which multi-gitter honours by never
// checking out the feature branch and pushing HEAD — the base branch — directly
// onto every selected repo. A test for this file has to pin what the keys SAY,
// not merely that they are said.
var occupiedConfigKeys = []struct {
	key string
	val string
	why string
}{
	{"token", `""`, "a static token is preferred over the GITHUB_TOKEN goldfinger sets, so PRs would be opened by an account goldfinger never announced; empty is what makes multi-gitter fall through to the environment, where goldfinger's credential travels"},
	{"username", `""`, "pairs with token as an alternative credential"},
	{"base-url", `""`, "would point the whole run at a different GitHub host"},
	{"platform", "github", "would switch the run to another forge entirely"},
	{"ssh-auth", "false", "would push over SSH keys instead of the resolved token — a different credential"},
	{"org", `[""]`, "a different flag than --repo, so a static one ADDS repos: PRs on repos never in the lockfile"},
	{"user", `[""]`, "as org"},
	{"group", `[""]`, "as org (GitLab)"},
	{"project", `[""]`, "as org (GitLab)"},
	{"topic", `[""]`, "as org"},
	{"skip-repo", `[""]`, "the same hole inverted — silently drops repos that WERE in the lockfile"},
	{"repo-search", `""`, "as org"},
	{"code-search", `""`, "as org"},
	{"repo-include", `""`, "filters the resolved set below the lockfile"},
	{"repo-exclude", `""`, "filters the resolved set below the lockfile"},
	{"skip-forks", "false", "filters the resolved set below the lockfile"},
	{"include-subgroups", "false", "widens the resolved set beyond the lockfile (GitLab)"},
	{"fork", "false", "would push the branch to a fork instead of the repo under review"},
	{"fork-owner", `""`, "would push the branch to another owner's fork"},
	{"skip-pr", "false", "the worst one: multi-gitter only checks out the feature branch when it is opening a PR, so skip-pr pushes HEAD — the BASE branch — turning a PR fanout into a direct push onto every repo's default branch"},
	{"push-only", "false", "pushes the branch and opens no PR, so a reviewed fanout silently produces nothing to review"},
	{"pr-auto-merge", "false", "would merge the PRs goldfinger opens without the human who is supposed to press merge"},
	{"dry-run", "false", "a static true would make a confirmed live run silently do nothing while goldfinger reports success"},
	{"interactive", "false", "would block on a per-repo prompt in a run whose output goldfinger captures and whose stdin is not a terminal"},
	{"manual-commit", "false", "expects the script to commit; goldfinger's generated script does not, so changes would go uncommitted"},
	{"conflict-strategy", "skip", "replace force-pushes over an existing branch; skip is what makes a part-failed batch safely re-runnable"},
	{"push-option", `[""]`, "passes server-side options like ci.skip through git push, and breaks the run outright under the default go-git"},
	{"api-push", "false", "only passed for --sign github, so a static true signs a --sign none/local run with GitHub's web-flow key instead"},
	{"git-type", "go", "only passed for --sign local, so a static cmd routes a --sign none run through the operator's git and their commit.gpgsign"},
	{"author-name", `""`, "setting an author makes multi-gitter reduce the commit env to GIT_AUTHOR/COMMITTER_*, stripping HOME/GPG_TTY and breaking the --sign local signing goldfinger announced"},
	{"author-email", `""`, "as author-name — and multi-gitter errors outright if only one of the pair is set"},
	{"base-branch", `""`, "goldfinger omits --base-branch so each PR targets that repo's own default branch; a static one silently retargets every PR, and the clone, at a single branch"},
	{"pr-body", `""`, "only passed when non-empty, so a static body would appear on PRs whose digest showed none"},
	{"draft", "false", "only passed when the operator asked for it, so a static true drafts PRs the digest reported as ready"},
	{"max-reviewers", "0", "a separate flag from reviewers, so it still binds when goldfinger names reviewers explicitly — and randomly shrinks the list the digest showed"},
	{"max-team-reviewers", "0", "as max-reviewers"},
	{"output", `"-"`, "multi-gitter writes its per-repo status block there, and that block is exactly what SummarizeDryRunOutput parses; a static path would empty the digest the human approves"},
}

// mustNotOccupyConfigKeys are the keys that meet the same rule and must stay OUT
// of the config, because for these occupancy is not neutral. multi-gitter reads
// them through a helper that returns nil for an unset flag, and its GitHub layer
// treats nil as "leave alone" but a non-nil empty slice as "make the PR match
// this" — so claiming the key removes every reviewer, label and assignee the PR
// already has (internal/scm/github/github.go setReviewers/setAssignees/setLabels).
//
// This is the inverse of occupiedConfigKeys and matters more: adding one of these
// looks like completing the list, and the damage — stripping the reviewers
// CODEOWNERS requested — is silent, remote, and on every run.
var mustNotOccupyConfigKeys = []string{"labels", "reviewers", "team-reviewers", "assignees"}

// TestApplyPinsNeutralisingConfig locks the config goldfinger hands multi-gitter.
// The subtlety worth a test is that an EMPTY file here would be inert: --config is
// layered above the static ~/.multi-gitter/config rather than replacing it, so the
// defence is occupying keys, not passing a file. The file must therefore exist,
// carry every key in occupiedConfigKeys, and be cleaned up afterwards.
func TestApplyPinsNeutralisingConfig(t *testing.T) {
	securityTest(t)
	var cap capture
	var content string
	var readErr error
	run := func(ctx context.Context, name string, args, env []string) ([]byte, error) {
		// Read from inside the run: Apply removes the file on return.
		for _, a := range args {
			if path, ok := strings.CutPrefix(a, "--config="); ok {
				var b []byte
				b, readErr = os.ReadFile(path) //nolint:gosec // G304: path is the temp file Apply just created, captured from its own argv.
				content = string(b)
			}
		}
		return cap.run(ctx, name, args, env)
	}
	_, err := Apply(context.Background(), run, twoRepoSelection(), baseSpec(), "t")
	require.NoError(t, err)

	var configPaths []string
	for _, a := range cap.args {
		if path, ok := strings.CutPrefix(a, "--config="); ok {
			configPaths = append(configPaths, path)
		}
	}
	require.Len(t, configPaths, 1, "apply must pass exactly one --config to multi-gitter")

	require.NoError(t, readErr, "the config must exist while multi-gitter runs")
	// Assert the exact line, key AND value. Key presence alone would pass a config
	// that claimed a key with a HARMFUL value — and since this file outranks the
	// host's, goldfinger would then be the one turning a PR fanout into a direct
	// push (skip-pr), signing with a key the run did not announce (api-push,
	// git-type), or making a confirmed live run do nothing (dry-run).
	for _, k := range occupiedConfigKeys {
		assert.Contains(t, content, "\n"+k.key+": "+k.val+"\n",
			"config must claim %q with its neutral value %q, otherwise the host's static config supplies it — or goldfinger's own does, which is worse: %s", k.key, k.val, k.why)
	}

	for _, k := range mustNotOccupyConfigKeys {
		assert.NotContains(t, content, "\n"+k+":",
			"config must NOT claim %q: multi-gitter reads it as a set-but-empty slice and strips every %s the PR already has", k, k)
	}

	_, statErr := os.Stat(configPaths[0])
	assert.True(t, os.IsNotExist(statErr), "temp config should be removed after Apply returns")
}

func TestApplyPropagatesRunError(t *testing.T) {
	run := func(context.Context, string, []string, []string) ([]byte, error) {
		return []byte("partial output"), errors.New("multi-gitter exploded")
	}
	result, err := Apply(context.Background(), run, twoRepoSelection(), baseSpec(), "t")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multi-gitter run")
	assert.Contains(t, err.Error(), "exploded")
	assert.Equal(t, []byte("partial output"), result.Output)
}

// multiCapture records every runner invocation, for batch tests.
type multiCapture struct {
	calls [][]string // args of each call
}

func (m *multiCapture) run(_ context.Context, _ string, args, _ []string) ([]byte, error) {
	cp := make([]string, len(args))
	copy(cp, args)
	m.calls = append(m.calls, cp)
	return []byte(fmt.Sprintf("batch-%d", len(m.calls))), nil
}

func fiveRepoSelection() models.Selection {
	s := models.Selection{Owner: "redscaresu", OwnerType: models.OwnerUser}
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		s.Repos = append(s.Repos, models.Repo{Owner: "redscaresu", Name: n})
	}
	return s
}

func repoFlags(args []string) []string {
	var repos []string
	for _, a := range args {
		if strings.HasPrefix(a, "--repo=") {
			repos = append(repos, strings.TrimPrefix(a, "--repo="))
		}
	}
	return repos
}

func TestApplyBatchesRunsAndPauses(t *testing.T) {
	var pauses []time.Duration
	orig := sleep
	sleep = func(d time.Duration) { pauses = append(pauses, d) }
	t.Cleanup(func() { sleep = orig })

	var mc multiCapture
	spec := baseSpec()
	spec.BatchSize = 2
	spec.BatchPause = 90 * time.Second
	result, err := Apply(context.Background(), mc.run, fiveRepoSelection(), spec, "t")
	require.NoError(t, err)

	// 5 repos / batch 2 -> 3 batches, split in order, none dropped.
	require.Len(t, mc.calls, 3)
	assert.Equal(t, []string{"redscaresu/a", "redscaresu/b"}, repoFlags(mc.calls[0]))
	assert.Equal(t, []string{"redscaresu/c", "redscaresu/d"}, repoFlags(mc.calls[1]))
	assert.Equal(t, []string{"redscaresu/e"}, repoFlags(mc.calls[2]))

	// Pause happens between batches only: 3 batches -> 2 pauses, each the set value.
	assert.Equal(t, []time.Duration{90 * time.Second, 90 * time.Second}, pauses)
	assert.Equal(t, "batch-1\nbatch-2\nbatch-3", string(result.Output))
}

func TestApplyNoBatchIsSingleRun(t *testing.T) {
	orig := sleep
	sleep = func(time.Duration) { t.Fatal("no pause expected without batching") }
	t.Cleanup(func() { sleep = orig })

	var mc multiCapture
	_, err := Apply(context.Background(), mc.run, fiveRepoSelection(), baseSpec(), "t")
	require.NoError(t, err)
	require.Len(t, mc.calls, 1, "unbatched apply is one run over the whole selection")
	assert.Len(t, repoFlags(mc.calls[0]), 5)
}

func TestApplyBatchErrorReportsBatchNumber(t *testing.T) {
	orig := sleep
	sleep = func(time.Duration) {}
	t.Cleanup(func() { sleep = orig })

	calls := 0
	run := func(context.Context, string, []string, []string) ([]byte, error) {
		calls++
		if calls == 2 {
			return []byte("batch two output"), errors.New("secondary rate limit")
		}
		return []byte(fmt.Sprintf("batch-%d\n", calls)), nil
	}
	spec := baseSpec()
	spec.BatchSize = 2
	result, err := Apply(context.Background(), run, fiveRepoSelection(), spec, "t")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "batch 2/3")
	assert.Contains(t, err.Error(), "rate limit")
	assert.Equal(t, "batch-1\nbatch two output", string(result.Output))
}

func TestApplySignModeArgs(t *testing.T) {
	securityTest(t)
	tests := []struct {
		mode      string
		wantArg   string   // the flag that must be present ("" = none of the below)
		absentArg []string // flags that must NOT be present
	}{
		{models.SignGitHub, "--api-push", []string{"--git-type=cmd"}},
		{models.SignLocal, "--git-type=cmd", []string{"--api-push"}},
		{models.SignNone, "", []string{"--api-push", "--git-type=cmd"}},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			var cap capture
			spec := baseSpec()
			spec.Sign = tt.mode
			_, err := Apply(context.Background(), cap.run, twoRepoSelection(), spec, "t")
			require.NoError(t, err)
			if tt.wantArg != "" {
				assert.Contains(t, cap.args, tt.wantArg)
			}
			for _, a := range tt.absentArg {
				assert.NotContains(t, cap.args, a)
			}
		})
	}
}

// TestApplyLocalSignPassesNoAuthorFlags locks the invariant that makes
// --sign=local sign at all: multi-gitter's --git-type=cmd honours the operator's
// commit.gpgsign ONLY while goldfinger passes no --author-name/--author-email —
// setting an author makes multi-gitter reduce the commit's env to GIT_AUTHOR/
// COMMITTER_* alone, stripping HOME/GPG_TTY and breaking signing. If a future
// change adds author flags to buildArgs, this fails loudly rather than shipping
// silently-unsigned commits under --sign=local.
func TestApplyLocalSignPassesNoAuthorFlags(t *testing.T) {
	securityTest(t)
	var cap capture
	spec := baseSpec()
	spec.Sign = models.SignLocal
	_, err := Apply(context.Background(), cap.run, twoRepoSelection(), spec, "t")
	require.NoError(t, err)

	for _, a := range cap.args {
		assert.False(t, strings.HasPrefix(a, "--author-name"),
			"--author-name breaks --sign=local GPG signing; got %q", a)
		assert.False(t, strings.HasPrefix(a, "--author-email"),
			"--author-email breaks --sign=local GPG signing; got %q", a)
	}
}

func TestChunk(t *testing.T) {
	repos := fiveRepoSelection().Repos
	assert.Len(t, chunk(repos, 0), 1, "size 0 = single chunk")
	assert.Len(t, chunk(repos, 10), 1, "size >= len = single chunk")
	assert.Len(t, chunk(repos, 2), 3)
	assert.Len(t, chunk(repos, 1), 5)
}

func TestShellQuoteEscapesSingleQuote(t *testing.T) {
	securityTest(t)
	assert.Equal(t, `'it'\''s'`, shellQuote("it's"))
}

func failRunner(t *testing.T) Runner {
	return func(context.Context, string, []string, []string) ([]byte, error) {
		t.Helper()
		t.Fatal("runner should not be called")
		return nil, nil
	}
}
