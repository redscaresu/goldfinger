package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// executeCmd runs the root command with the given token and args, capturing
// combined output. A token of "" simulates a missing PAT. The local gh fallback
// is stubbed off so tests exercise the env-var path deterministically, without
// depending on whether the test host happens to be logged into gh.
func executeCmd(t *testing.T, token string, args ...string) (string, error) {
	t.Helper()
	t.Setenv(tokenEnvVar, token)
	stubGhToken(t, "", false)
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
		{"select", "--help"},
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

func TestSelectValidation(t *testing.T) {
	// Only error paths are exercised here: they all return before any network
	// call. The valid-targeting happy path resolves real repos, so it is covered
	// by client's httptest tests and the live smoke test, not here.
	tests := []struct {
		name    string
		token   string
		args    []string
		wantErr string
	}{
		{
			name:    "missing token",
			token:   "",
			args:    []string{"select", "--org", "acme", "--all-repos"},
			wantErr: "GOLD_FINGER_PAT",
		},
		{
			name:    "missing org",
			token:   "ghp_x",
			args:    []string{"select", "--all-repos"},
			wantErr: "--org is required",
		},
		{
			name:    "neither all-repos nor topic",
			token:   "ghp_x",
			args:    []string{"select", "--org", "acme"},
			wantErr: "one of --all-repos or --topic",
		},
		{
			name:    "both all-repos and topic",
			token:   "ghp_x",
			args:    []string{"select", "--org", "acme", "--all-repos", "--topic", "platform"},
			wantErr: "mutually exclusive",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := executeCmd(t, tt.token, tt.args...)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}
