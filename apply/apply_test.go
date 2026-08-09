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

func TestApplyPinsEmptyConfig(t *testing.T) {
	securityTest(t)
	// multi-gitter is pointed at a goldfinger-owned empty config so host config
	// discovery can't override the lockfile selection. The file must exist at
	// call time and be cleaned up afterwards.
	var cap capture
	_, err := Apply(context.Background(), cap.run, twoRepoSelection(), baseSpec(), "t")
	require.NoError(t, err)

	var configPath string
	for _, a := range cap.args {
		if strings.HasPrefix(a, "--config=") {
			configPath = strings.TrimPrefix(a, "--config=")
		}
	}
	require.NotEmpty(t, configPath, "apply must pass an explicit --config to multi-gitter")
	_, statErr := os.Stat(configPath)
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
