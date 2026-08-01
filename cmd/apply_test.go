package main

import (
	"testing"

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
