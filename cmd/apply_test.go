package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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
		sign:          "none",
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
		{"missing sign", func(a *applyValidation) { a.sign = "" }, "--sign is required"},
		{"invalid sign", func(a *applyValidation) { a.sign = "gpg" }, "--sign \"gpg\" is invalid"},
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
	spec := models.ApplySpec{Branch: "b", CommitMessage: "m", PRTitle: "t", Script: []string{"true"}, DryRun: true, Sign: models.SignNone}

	t.Run("dry-run frames and delegates", func(t *testing.T) {
		var gotArgs []string
		run := func(_ context.Context, _ string, args, _ []string) error { gotArgs = args; return nil }
		var errOut bytes.Buffer
		err := runApply(context.Background(), run, sel, spec, "tok", false, io.Discard, &errOut)
		require.NoError(t, err)
		assert.Contains(t, errOut.String(), "Applying to 1 repo(s)")
		assert.Contains(t, errOut.String(), "dry-run")
		assert.Contains(t, errOut.String(), "apply complete")
		assert.Contains(t, gotArgs, "--dry-run")
	})

	t.Run("live mode banner", func(t *testing.T) {
		live := spec
		live.DryRun = false
		live.Confirm = true // Apply refuses a live run that isn't confirmed
		var errOut bytes.Buffer
		run := func(_ context.Context, _ string, _, _ []string) error { return nil }
		require.NoError(t, runApply(context.Background(), run, sel, live, "tok", false, io.Discard, &errOut))
		assert.Contains(t, errOut.String(), "LIVE")
	})

	t.Run("propagates delegate error", func(t *testing.T) {
		run := func(_ context.Context, _ string, _, _ []string) error { return errors.New("mg blew up") }
		err := runApply(context.Background(), run, sel, spec, "tok", false, io.Discard, &bytes.Buffer{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "mg blew up")
	})
}

func TestRunApplyPlanJSON(t *testing.T) {
	sel := models.Selection{Owner: "acme", Repos: []models.Repo{
		{Owner: "acme", Name: "a", DefaultBranch: "main"},
		{Owner: "acme", Name: "b", DefaultBranch: "dev"},
	}}
	spec := models.ApplySpec{
		Branch: "bump", CommitMessage: "chore: bump", PRTitle: "Bump", PRBody: "some body",
		Labels: []string{"deps"}, Reviewers: []string{"acme/team"}, Draft: true,
		Script: []string{"sed", "-i", "s/x/y/", "Dockerfile"}, DryRun: true, Sign: models.SignLocal,
		BatchSize: 5, BatchPause: 60 * 1e9, // 60s
	}

	var out, errOut bytes.Buffer
	called := false
	run := func(_ context.Context, _ string, args, _ []string) error {
		called = true
		assert.Contains(t, args, "--dry-run", "plan-json supplements, never replaces, the dry-run")
		return nil
	}
	require.NoError(t, runApply(context.Background(), run, sel, spec, "tok", true, &out, &errOut))
	assert.True(t, called, "apply.Apply still runs — plan-json is not a short-circuit")

	// stdout is the plan JSON only; banners went to stderr.
	var plan applyPlan
	require.NoError(t, json.Unmarshal(out.Bytes(), &plan))
	assert.Equal(t, applyPlanVersion, plan.Version)
	assert.True(t, plan.DryRun)
	assert.Equal(t, models.SignLocal, plan.SignMode)
	assert.Equal(t, "bump", plan.Branch)
	assert.True(t, plan.PRBodyPresent, "body reduced to a presence bool")
	assert.True(t, plan.Draft)
	// Command redacted to argv[0]; no script args leak into the plan.
	assert.Equal(t, "sed", plan.CommandProgram)
	assert.True(t, plan.CommandRedacted)
	assert.NotContains(t, out.String(), "Dockerfile")
	require.NotNil(t, plan.BatchSize)
	assert.Equal(t, 5, *plan.BatchSize)
	require.NotNil(t, plan.BatchPause)
	assert.Equal(t, "1m0s", *plan.BatchPause)
	assert.Equal(t, "per-repo-default", plan.BaseBranchSrc)
	assert.Equal(t, 2, plan.ReposTotal)
	require.Len(t, plan.Repos, 2)
	assert.Equal(t, "acme/a", plan.Repos[0].Repo)
	assert.Equal(t, "main", plan.Repos[0].BaseBranchRecorded)
	assert.Equal(t, "dev", plan.Repos[1].BaseBranchRecorded)

	// The banner (stderr) never carries the plan JSON.
	assert.NotContains(t, errOut.String(), "\"version\"")

	t.Run("explicit base-branch source", func(t *testing.T) {
		s2 := spec
		s2.BaseBranch = "release"
		var out2 bytes.Buffer
		require.NoError(t, runApply(context.Background(), run, sel, s2, "tok", true, &out2, &bytes.Buffer{}))
		var p2 applyPlan
		require.NoError(t, json.Unmarshal(out2.Bytes(), &p2))
		assert.Equal(t, "explicit:release", p2.BaseBranchSrc)
		assert.Equal(t, "release", p2.Repos[0].BaseBranchRecorded)
	})
}

// A plan with no labels/reviewers must serialise them as [] rather than null, so
// a machine consumer parsing the documented array fields never trips over a null.
func TestBuildApplyPlanNormalisesNilLists(t *testing.T) {
	sel := models.Selection{Owner: "acme", Repos: []models.Repo{{Owner: "acme", Name: "a", DefaultBranch: "main"}}}
	spec := models.ApplySpec{Branch: "x", CommitMessage: "m", PRTitle: "t", Script: []string{"true"}, Sign: models.SignNone}

	plan := buildApplyPlan(sel, spec)
	require.NotNil(t, plan.Labels, "nil labels must normalise to an empty slice")
	require.NotNil(t, plan.Reviewers, "nil reviewers must normalise to an empty slice")

	data, err := json.Marshal(plan)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"labels":[]`)
	assert.Contains(t, string(data), `"reviewers":[]`)
	assert.NotContains(t, string(data), `"labels":null`)
	assert.NotContains(t, string(data), `"reviewers":null`)
}

func TestRunApplyPrintsPerRepoBase(t *testing.T) {
	sel := models.Selection{Owner: "acme", Repos: []models.Repo{
		{Owner: "acme", Name: "a", DefaultBranch: "main"},
		{Owner: "acme", Name: "b", DefaultBranch: "dev"},
	}}
	run := func(_ context.Context, _ string, _, _ []string) error { return nil }

	t.Run("routes each repo to its own default branch", func(t *testing.T) {
		spec := models.ApplySpec{Branch: "x", CommitMessage: "m", PRTitle: "t", Script: []string{"true"}, DryRun: true, Sign: models.SignNone}
		var errOut bytes.Buffer
		require.NoError(t, runApply(context.Background(), run, sel, spec, "tok", false, io.Discard, &errOut))
		assert.Contains(t, errOut.String(), "acme/a -> main")
		assert.Contains(t, errOut.String(), "acme/b -> dev")
	})

	t.Run("global base-branch overrides every repo", func(t *testing.T) {
		spec := models.ApplySpec{Branch: "x", BaseBranch: "release", CommitMessage: "m", PRTitle: "t", Script: []string{"true"}, DryRun: true, Sign: models.SignNone}
		var errOut bytes.Buffer
		require.NoError(t, runApply(context.Background(), run, sel, spec, "tok", false, io.Discard, &errOut))
		assert.Contains(t, errOut.String(), "acme/a -> release")
		assert.Contains(t, errOut.String(), "acme/b -> release")
	})
}

func TestRunApplySigningBanner(t *testing.T) {
	sel := models.Selection{Owner: "acme", Repos: []models.Repo{{Owner: "acme", Name: "a"}}}
	run := func(_ context.Context, _ string, _, _ []string) error { return nil }

	tests := []struct {
		mode string
		want string
	}{
		{models.SignLocal, "signed with your GPG key"},
		{models.SignGitHub, "GitHub-verified"},
		{models.SignNone, "UNSIGNED"},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			spec := models.ApplySpec{Branch: "b", CommitMessage: "m", PRTitle: "t", Script: []string{"true"}, DryRun: true, Sign: tt.mode}
			var errOut bytes.Buffer
			require.NoError(t, runApply(context.Background(), run, sel, spec, "tok", false, io.Discard, &errOut))
			assert.Contains(t, errOut.String(), "signing: "+tt.mode)
			assert.Contains(t, errOut.String(), tt.want)
		})
	}
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
// they are deterministic regardless of whether multi-gitter is installed. The
// pure flag guards fail before auth; the missing-token case supplies valid flags
// and a readable selection so it reaches resolveToken (which precedes the
// multi-gitter probe, keeping this tool-independent).
func TestApplyCmdGuards(t *testing.T) {
	t.Run("missing token", func(t *testing.T) {
		sel := writeSelection(t)
		_, err := executeCmd(t, "", "apply", "--branch", "b", "--commit-message", "m",
			"--pr-title", "t", "--sign", "none", "--selection", sel, "--", "true")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "GOLD_FINGER_PAT")
	})

	t.Run("missing script separator", func(t *testing.T) {
		_, err := executeCmd(t, "tok", "apply", "--branch", "b", "--commit-message", "m", "--pr-title", "t", "--sign", "none")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "after --")
	})

	t.Run("missing sign is refused", func(t *testing.T) {
		_, err := executeCmd(t, "tok", "apply", "--branch", "b", "--commit-message", "m", "--pr-title", "t", "--", "true")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--sign is required")
	})

	t.Run("invalid sign is refused", func(t *testing.T) {
		_, err := executeCmd(t, "tok", "apply", "--branch", "b", "--commit-message", "m",
			"--pr-title", "t", "--sign", "pgp", "--", "true")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--sign \"pgp\" is invalid")
	})

	t.Run("pr-body and pr-body-file are mutually exclusive", func(t *testing.T) {
		_, err := executeCmd(t, "tok", "apply", "--branch", "b", "--commit-message", "m",
			"--pr-title", "t", "--sign", "none", "--pr-body", "x", "--pr-body-file", "y.md", "--", "true")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "mutually exclusive")
	})

	t.Run("real run without confirm is refused", func(t *testing.T) {
		_, err := executeCmd(t, "tok", "apply", "--branch", "b", "--commit-message", "m",
			"--pr-title", "t", "--sign", "none", "--dry-run=false", "--", "true")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--confirm")
	})
}
