package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubGhToken swaps the local-gh fallback for a fixed result for one test,
// restoring the real lookup afterwards. It lets token tests run identically
// whether or not the host is logged into gh.
func stubGhToken(t *testing.T, token string, ok bool) {
	t.Helper()
	old := ghTokenLookup
	ghTokenLookup = func() (string, bool) { return token, ok }
	t.Cleanup(func() { ghTokenLookup = old })
}

func TestResolveToken(t *testing.T) {
	t.Run("env var wins over gh session", func(t *testing.T) {
		t.Setenv(tokenEnvVar, "pat-from-env")
		stubGhToken(t, "token-from-gh", true)
		got, err := resolveToken()
		require.NoError(t, err)
		assert.Equal(t, "pat-from-env", got)
	})

	t.Run("falls back to local gh session when env unset", func(t *testing.T) {
		t.Setenv(tokenEnvVar, "")
		stubGhToken(t, "token-from-gh", true)
		got, err := resolveToken()
		require.NoError(t, err)
		assert.Equal(t, "token-from-gh", got)
	})

	t.Run("errors naming both options when neither yields a token", func(t *testing.T) {
		t.Setenv(tokenEnvVar, "")
		stubGhToken(t, "", false)
		_, err := resolveToken()
		require.Error(t, err)
		assert.Contains(t, err.Error(), tokenEnvVar)
		assert.Contains(t, err.Error(), "gh auth login")
	})
}

func TestValidateTargeting(t *testing.T) {
	tests := []struct {
		name    string
		in      targeting
		wantErr string
	}{
		{
			name: "all-repos ok",
			in:   targeting{org: "acme", allRepos: true},
		},
		{
			name: "topic ok",
			in:   targeting{org: "acme", topics: []string{"platform"}},
		},
		{
			name:    "missing org",
			in:      targeting{allRepos: true},
			wantErr: "--org is required",
		},
		{
			name:    "both all-repos and topic",
			in:      targeting{org: "acme", allRepos: true, topics: []string{"platform"}},
			wantErr: "mutually exclusive",
		},
		{
			name:    "neither all-repos nor topic",
			in:      targeting{org: "acme"},
			wantErr: "one of --all-repos or --topic",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTargeting(tt.in)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}
