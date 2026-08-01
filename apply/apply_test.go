package apply

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

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

	err := Apply(context.Background(), cap.run, twoRepoSelection(), spec, "secret-token")
	require.NoError(t, err)

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

func TestApplyPropagatesRunError(t *testing.T) {
	run := func(context.Context, string, []string, []string) error {
		return errors.New("multi-gitter exploded")
	}
	err := Apply(context.Background(), run, twoRepoSelection(), baseSpec(), "t")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multi-gitter run")
	assert.Contains(t, err.Error(), "exploded")
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
