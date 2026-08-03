package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/redscaresu/goldfinger/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateApply(t *testing.T) {
	ok := applyValidation{
		branch:        "bump",
		commitMessage: "bump image",
		prTitle:       "Bump image",
		script:        []string{"true"},
	}
	require.NoError(t, validateApply(ok))

	tests := []struct {
		name    string
		mutate  func(*applyValidation)
		wantErr string
	}{
		{"missing branch", func(a *applyValidation) { a.branch = "" }, "--branch is required"},
		{"missing commit message", func(a *applyValidation) { a.commitMessage = "" }, "--commit-message is required"},
		{"missing pr title", func(a *applyValidation) { a.prTitle = "" }, "--pr-title is required"},
		{"missing script", func(a *applyValidation) { a.script = nil }, "after --"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			av := ok
			tt.mutate(&av)
			err := validateApply(av)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestRunApply(t *testing.T) {
	sel := models.Selection{Owner: "acme", Repos: []models.Repo{{Owner: "acme", Name: "a"}}}
	spec := models.ApplySpec{Branch: "b", CommitMessage: "m", PRTitle: "t", Script: []string{"true"}, DryRun: true}

	t.Run("dry-run frames and delegates", func(t *testing.T) {
		var gotArgs []string
		run := func(_ context.Context, _ string, args, _ []string) error { gotArgs = args; return nil }
		var errOut bytes.Buffer
		err := runApply(context.Background(), run, sel, spec, "tok", &errOut)
		require.NoError(t, err)
		assert.Contains(t, errOut.String(), "Applying to 1 repo(s)")
		assert.Contains(t, errOut.String(), "dry-run")
		assert.Contains(t, errOut.String(), "apply complete")
		assert.Contains(t, gotArgs, "--dry-run")
	})

	t.Run("live mode banner", func(t *testing.T) {
		live := spec
		live.DryRun = false
		var errOut bytes.Buffer
		run := func(_ context.Context, _ string, _, _ []string) error { return nil }
		require.NoError(t, runApply(context.Background(), run, sel, live, "tok", &errOut))
		assert.Contains(t, errOut.String(), "LIVE")
	})

	t.Run("propagates delegate error", func(t *testing.T) {
		run := func(_ context.Context, _ string, _, _ []string) error { return errors.New("mg blew up") }
		err := runApply(context.Background(), run, sel, spec, "tok", &bytes.Buffer{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "mg blew up")
	})
}

func TestRunApplyPrintsPerRepoBase(t *testing.T) {
	sel := models.Selection{Owner: "acme", Repos: []models.Repo{
		{Owner: "acme", Name: "a", DefaultBranch: "main"},
		{Owner: "acme", Name: "b", DefaultBranch: "dev"},
	}}
	run := func(_ context.Context, _ string, _, _ []string) error { return nil }

	t.Run("routes each repo to its own default branch", func(t *testing.T) {
		spec := models.ApplySpec{Branch: "x", CommitMessage: "m", PRTitle: "t", Script: []string{"true"}, DryRun: true}
		var errOut bytes.Buffer
		require.NoError(t, runApply(context.Background(), run, sel, spec, "tok", &errOut))
		assert.Contains(t, errOut.String(), "acme/a -> main")
		assert.Contains(t, errOut.String(), "acme/b -> dev")
	})

	t.Run("global base-branch overrides every repo", func(t *testing.T) {
		spec := models.ApplySpec{Branch: "x", BaseBranch: "release", CommitMessage: "m", PRTitle: "t", Script: []string{"true"}, DryRun: true}
		var errOut bytes.Buffer
		require.NoError(t, runApply(context.Background(), run, sel, spec, "tok", &errOut))
		assert.Contains(t, errOut.String(), "acme/a -> release")
		assert.Contains(t, errOut.String(), "acme/b -> release")
	})
}

func TestResolveBase(t *testing.T) {
	repo := models.Repo{Owner: "acme", Name: "a", DefaultBranch: "dev"}
	assert.Equal(t, "release", resolveBase("release", repo))
	assert.Equal(t, "dev", resolveBase("", repo))
	assert.Equal(t, "repo default", resolveBase("", models.Repo{Owner: "acme", Name: "a"}))
}

func TestResolvePRBody(t *testing.T) {
	t.Run("inline body passes through", func(t *testing.T) {
		got, err := resolvePRBody("hello", "")
		require.NoError(t, err)
		assert.Equal(t, "hello", got)
	})

	t.Run("reads from file", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "body.md")
		require.NoError(t, os.WriteFile(f, []byte("from file"), 0o644))
		got, err := resolvePRBody("", f)
		require.NoError(t, err)
		assert.Equal(t, "from file", got)
	})

	t.Run("both set is an error", func(t *testing.T) {
		_, err := resolvePRBody("inline", "/some/path")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "mutually exclusive")
	})

	t.Run("missing file errors", func(t *testing.T) {
		_, err := resolvePRBody("", filepath.Join(t.TempDir(), "nope.md"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "pr-body-file")
	})
}

// The apply command's guard paths all return before any tool/network use, so
// they are deterministic regardless of whether multi-gitter is installed.
func TestApplyCmdGuards(t *testing.T) {
	t.Run("missing token", func(t *testing.T) {
		_, err := executeCmd(t, "", "apply", "--branch", "b", "--commit-message", "m", "--pr-title", "t", "--", "true")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "GOLD_FINGER_PAT")
	})

	t.Run("missing script separator", func(t *testing.T) {
		_, err := executeCmd(t, "tok", "apply", "--branch", "b", "--commit-message", "m", "--pr-title", "t")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "after --")
	})

	t.Run("pr-body and pr-body-file are mutually exclusive", func(t *testing.T) {
		_, err := executeCmd(t, "tok", "apply", "--branch", "b", "--commit-message", "m",
			"--pr-title", "t", "--pr-body", "x", "--pr-body-file", "y.md", "--", "true")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "mutually exclusive")
	})

	t.Run("real run without confirm is refused", func(t *testing.T) {
		_, err := executeCmd(t, "tok", "apply", "--branch", "b", "--commit-message", "m",
			"--pr-title", "t", "--dry-run=false", "--", "true")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--confirm")
	})
}
