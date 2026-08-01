package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// executeCmd runs the root command with the given token and args, capturing
// combined output. A token of "" simulates a missing GITHUB_TOKEN.
func executeCmd(t *testing.T, token string, args ...string) (string, error) {
	t.Helper()
	t.Setenv("GITHUB_TOKEN", token)
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)
	err := root.Execute()
	return buf.String(), err
}

func TestHelpRenders(t *testing.T) {
	for _, args := range [][]string{
		{"--help"},
		{"repos", "--help"},
		{"run", "--help"},
	} {
		out, err := executeCmd(t, "", args...)
		require.NoError(t, err)
		assert.Contains(t, out, "goldfinger")
	}
}

func TestVersionRenders(t *testing.T) {
	out, err := executeCmd(t, "", "--version")
	require.NoError(t, err)
	assert.Contains(t, out, "dev")
}

func TestReposValidation(t *testing.T) {
	tests := []struct {
		name    string
		token   string
		args    []string
		wantErr string
		wantOut string
	}{
		{
			name:    "missing token",
			token:   "",
			args:    []string{"repos", "--org", "acme", "--all-repos"},
			wantErr: "GITHUB_TOKEN",
		},
		{
			name:    "missing org",
			token:   "ghp_x",
			args:    []string{"repos", "--all-repos"},
			wantErr: "--org is required",
		},
		{
			name:    "neither all-repos nor topic",
			token:   "ghp_x",
			args:    []string{"repos", "--org", "acme"},
			wantErr: "one of --all-repos or --topic",
		},
		{
			name:    "both all-repos and topic",
			token:   "ghp_x",
			args:    []string{"repos", "--org", "acme", "--all-repos", "--topic", "platform"},
			wantErr: "mutually exclusive",
		},
		{
			name:    "valid targeting",
			token:   "ghp_x",
			args:    []string{"repos", "--org", "acme", "--all-repos"},
			wantOut: "step 2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := executeCmd(t, tt.token, tt.args...)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Contains(t, out, tt.wantOut)
		})
	}
}

func TestRunValidation(t *testing.T) {
	base := []string{"run", "--org", "acme", "--all-repos"}

	t.Run("missing script separator", func(t *testing.T) {
		args := append([]string{}, base...)
		args = append(args, "--branch", "b", "--commit-message", "m", "--pr-title", "t")
		_, err := executeCmd(t, "ghp_x", args...)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "after --")
	})

	t.Run("missing branch", func(t *testing.T) {
		args := append([]string{}, base...)
		args = append(args, "--commit-message", "m", "--pr-title", "t", "--", "true")
		_, err := executeCmd(t, "ghp_x", args...)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--branch is required")
	})

	t.Run("valid run", func(t *testing.T) {
		args := append([]string{}, base...)
		args = append(args, "--branch", "b", "--commit-message", "m", "--pr-title", "t", "--", "sed", "-i", "s/a/b/", "Dockerfile")
		out, err := executeCmd(t, "ghp_x", args...)
		require.NoError(t, err)
		assert.Contains(t, out, "step 4")
	})

	t.Run("invalid output format", func(t *testing.T) {
		args := append([]string{}, base...)
		args = append(args, "--branch", "b", "--commit-message", "m", "--pr-title", "t", "--output", "yaml", "--", "true")
		_, err := executeCmd(t, "ghp_x", args...)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--output")
	})
}
