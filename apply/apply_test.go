package apply

import (
	"context"
	"errors"
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
	}
}

type capture struct {
	name string
	args []string
	env  []string
}

func (c *capture) run(_ context.Context, name string, args, env []string) error {
	c.name, c.args, c.env = name, args, env
	return nil
}

func TestApplyInvocation(t *testing.T) {
	var cap capture
	spec := baseSpec()
	spec.PRBody = "details"
	spec.Labels = []string{"fleet", "chore"}
	spec.Reviewers = []string{"redscaresu"}
	spec.Draft = true
	spec.BaseBranch = "dev"

	err := Apply(context.Background(), cap.run, twoRepoSelection(), spec, "secret-token")
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
	require.NoError(t, Apply(context.Background(), cap.run, twoRepoSelection(), baseSpec(), "t"))
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
	run := func(_ context.Context, _ string, args, _ []string) error {
		scriptPath = args[1] // run <scriptPath>
		data, err := os.ReadFile(scriptPath)
		require.NoError(t, err)
		scriptBody = string(data)
		return nil
	}
	require.NoError(t, Apply(context.Background(), run, twoRepoSelection(), baseSpec(), "t"))

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
	require.NoError(t, Apply(context.Background(), cap.run, twoRepoSelection(), spec, "t"))
	assert.NotContains(t, cap.args, "--dry-run")
}

func TestApplyEmptySelection(t *testing.T) {
	err := Apply(context.Background(), failRunner(t), models.Selection{Owner: "x"}, baseSpec(), "t")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

func TestApplyTooManyRepos(t *testing.T) {
	s := models.Selection{Owner: "big"}
	for i := 0; i <= maxRepos; i++ {
		s.Repos = append(s.Repos, models.Repo{Owner: "big", Name: "r"})
	}
	err := Apply(context.Background(), failRunner(t), s, baseSpec(), "t")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "limit")
}

func TestApplyOverridesExistingToken(t *testing.T) {
	t.Setenv(tokenEnv, "runner-default-token") // e.g. CI's own GITHUB_TOKEN
	var cap capture
	require.NoError(t, Apply(context.Background(), cap.run, twoRepoSelection(), baseSpec(), "our-token"))

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
	t.Setenv(models.TokenEnvVar, "raw-pat") // operator's exported GOLD_FINGER_PAT
	var cap capture
	require.NoError(t, Apply(context.Background(), cap.run, twoRepoSelection(), baseSpec(), "mapped-token"))

	for _, e := range cap.env {
		assert.NotContains(t, e, models.TokenEnvVar+"=", "source PAT var must not reach the child")
		assert.NotContains(t, e, "raw-pat", "raw PAT value must not reach the child under any name")
	}
	assert.Contains(t, cap.env, tokenEnv+"=mapped-token")
}

func TestApplyPinsEmptyConfig(t *testing.T) {
	// multi-gitter is pointed at a goldfinger-owned empty config so host config
	// discovery can't override the lockfile selection. The file must exist at
	// call time and be cleaned up afterwards.
	var cap capture
	require.NoError(t, Apply(context.Background(), cap.run, twoRepoSelection(), baseSpec(), "t"))

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
	run := func(context.Context, string, []string, []string) error {
		return errors.New("multi-gitter exploded")
	}
	err := Apply(context.Background(), run, twoRepoSelection(), baseSpec(), "t")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multi-gitter run")
	assert.Contains(t, err.Error(), "exploded")
}

// multiCapture records every runner invocation, for batch tests.
type multiCapture struct {
	calls [][]string // args of each call
}

func (m *multiCapture) run(_ context.Context, _ string, args, _ []string) error {
	cp := make([]string, len(args))
	copy(cp, args)
	m.calls = append(m.calls, cp)
	return nil
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
	require.NoError(t, Apply(context.Background(), mc.run, fiveRepoSelection(), spec, "t"))

	// 5 repos / batch 2 -> 3 batches, split in order, none dropped.
	require.Len(t, mc.calls, 3)
	assert.Equal(t, []string{"redscaresu/a", "redscaresu/b"}, repoFlags(mc.calls[0]))
	assert.Equal(t, []string{"redscaresu/c", "redscaresu/d"}, repoFlags(mc.calls[1]))
	assert.Equal(t, []string{"redscaresu/e"}, repoFlags(mc.calls[2]))

	// Pause happens between batches only: 3 batches -> 2 pauses, each the set value.
	assert.Equal(t, []time.Duration{90 * time.Second, 90 * time.Second}, pauses)
}

func TestApplyNoBatchIsSingleRun(t *testing.T) {
	orig := sleep
	sleep = func(time.Duration) { t.Fatal("no pause expected without batching") }
	t.Cleanup(func() { sleep = orig })

	var mc multiCapture
	require.NoError(t, Apply(context.Background(), mc.run, fiveRepoSelection(), baseSpec(), "t"))
	require.Len(t, mc.calls, 1, "unbatched apply is one run over the whole selection")
	assert.Len(t, repoFlags(mc.calls[0]), 5)
}

func TestApplyBatchErrorReportsBatchNumber(t *testing.T) {
	orig := sleep
	sleep = func(time.Duration) {}
	t.Cleanup(func() { sleep = orig })

	calls := 0
	run := func(context.Context, string, []string, []string) error {
		calls++
		if calls == 2 {
			return errors.New("secondary rate limit")
		}
		return nil
	}
	spec := baseSpec()
	spec.BatchSize = 2
	err := Apply(context.Background(), run, fiveRepoSelection(), spec, "t")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "batch 2/3")
	assert.Contains(t, err.Error(), "rate limit")
}

func TestChunk(t *testing.T) {
	repos := fiveRepoSelection().Repos
	assert.Len(t, chunk(repos, 0), 1, "size 0 = single chunk")
	assert.Len(t, chunk(repos, 10), 1, "size >= len = single chunk")
	assert.Len(t, chunk(repos, 2), 3)
	assert.Len(t, chunk(repos, 1), 5)
}

func TestShellQuoteEscapesSingleQuote(t *testing.T) {
	assert.Equal(t, `'it'\''s'`, shellQuote("it's"))
}

func failRunner(t *testing.T) Runner {
	return func(context.Context, string, []string, []string) error {
		t.Helper()
		t.Fatal("runner should not be called")
		return nil
	}
}
