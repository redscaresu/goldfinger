package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateToken(t *testing.T) {
	require.NoError(t, validateToken("ghp_xxx"))

	err := validateToken("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), tokenEnvVar)
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
