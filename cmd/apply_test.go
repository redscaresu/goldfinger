package main

import (
	"bytes"
	"context"
	"errors"
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

	t.Run("real run without confirm is refused", func(t *testing.T) {
		_, err := executeCmd(t, "tok", "apply", "--branch", "b", "--commit-message", "m",
			"--pr-title", "t", "--dry-run=false", "--", "true")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--confirm")
	})
}
