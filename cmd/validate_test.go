package main

import (
	"bytes"
	"context"
	"strings"
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
	ghTokenLookup = func(context.Context) (string, bool) { return token, ok }
	t.Cleanup(func() { ghTokenLookup = old })
}

func TestResolveToken(t *testing.T) {
	t.Run("env var wins over gh session", func(t *testing.T) {
		t.Setenv(tokenEnvVar, "pat-from-env")
		stubGhToken(t, "token-from-gh", true)
		got, source, err := resolveToken(context.Background())
		require.NoError(t, err)
		assert.Equal(t, "pat-from-env", got)
		assert.Equal(t, tokenSourceEnv, source)
	})

	t.Run("falls back to local gh session when env unset", func(t *testing.T) {
		t.Setenv(tokenEnvVar, "")
		stubGhToken(t, "token-from-gh", true)
		got, source, err := resolveToken(context.Background())
		require.NoError(t, err)
		assert.Equal(t, "token-from-gh", got)
		assert.Equal(t, tokenSourceGh, source)
	})

	t.Run("errors naming both options when neither yields a token", func(t *testing.T) {
		t.Setenv(tokenEnvVar, "")
		stubGhToken(t, "", false)
		_, _, err := resolveToken(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), tokenEnvVar)
		assert.Contains(t, err.Error(), "gh auth login")
	})
}

func TestValidateMirror(t *testing.T) {
	tests := []struct {
		name    string
		in      mirrorValidation
		wantErr string
	}{
		{
			name: "no branch, no depth",
			in:   mirrorValidation{},
		},
		{
			name: "branch without shallow depth",
			in:   mirrorValidation{branch: "dev"},
		},
		{
			name: "shallow depth without branch",
			in:   mirrorValidation{cloneDepth: 1},
		},
		{
			name: "branch with explicit full depth",
			in:   mirrorValidation{branch: "dev", cloneDepth: 0},
		},
		{
			name:    "branch with shallow depth",
			in:      mirrorValidation{branch: "dev", cloneDepth: 1},
			wantErr: "--clone-depth",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateMirror(tt.in)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestValidateExpectSelectionDigest(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr string
	}{
		{name: "empty passes as no-check", in: "", want: ""},
		{name: "lowercase 64-hex passes", in: strings.Repeat("a", 64), want: strings.Repeat("a", 64)},
		{name: "uppercase normalises to lowercase", in: strings.Repeat("A", 64), want: strings.Repeat("a", 64)},
		{name: "too short", in: strings.Repeat("a", 63), wantErr: "64-character"},
		{name: "too long", in: strings.Repeat("a", 65), wantErr: "64-character"},
		{name: "non-hex char", in: strings.Repeat("g", 64), wantErr: "hexadecimal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateExpectSelectionDigest(tt.in)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestAnnounceTokenSource(t *testing.T) {
	// Clear ambient tokens so this exercises the plain source line (the CI/host
	// env may itself set GITHUB_TOKEN, which would otherwise trip the warning).
	for _, v := range ambientTokenVars {
		t.Setenv(v, "")
	}
	var buf bytes.Buffer
	announceTokenSource(&buf, tokenSourceGh)
	assert.Equal(t, "auth: using local gh session (gh auth token)\n", buf.String())
}

func TestAmbientTokenWarning(t *testing.T) {
	t.Run("gh source with ambient token warns", func(t *testing.T) {
		for _, v := range ambientTokenVars {
			t.Setenv(v, "")
		}
		t.Setenv("GITHUB_TOKEN", "ghp_ambient")
		warn := ambientTokenWarning(tokenSourceGh)
		require.NotEmpty(t, warn)
		assert.Contains(t, warn, "GITHUB_TOKEN")
		assert.Contains(t, warn, tokenEnvVar)
	})

	t.Run("PAT source never warns", func(t *testing.T) {
		t.Setenv("GITHUB_TOKEN", "ghp_ambient")
		// GOLD_FINGER_PAT is goldfinger's own explicit input, so an ambient token
		// is irrelevant to its resolution and must not raise a warning.
		assert.Empty(t, ambientTokenWarning(tokenSourceEnv))
	})

	t.Run("gh source without ambient token is quiet", func(t *testing.T) {
		for _, v := range ambientTokenVars {
			t.Setenv(v, "")
		}
		assert.Empty(t, ambientTokenWarning(tokenSourceGh))
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
			wantErr: "one of --all-repos, --topic, or --repo/--repos-from is required",
		},
		{
			name: "explicit repo ok",
			in:   targeting{org: "acme", repos: []string{"svc-a"}},
		},
		{
			name: "explicit repos-from ok",
			in:   targeting{org: "acme", reposFrom: "repos.txt"},
		},
		{
			name:    "explicit and topic are mutually exclusive",
			in:      targeting{org: "acme", topics: []string{"platform"}, repos: []string{"svc-a"}},
			wantErr: "mutually exclusive",
		},
		{
			name:    "explicit and all-repos are mutually exclusive",
			in:      targeting{org: "acme", allRepos: true, repos: []string{"svc-a"}},
			wantErr: "mutually exclusive",
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
