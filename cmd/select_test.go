package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/redscaresu/goldfinger/models"
	"github.com/redscaresu/goldfinger/selection"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeResolver struct {
	login     string
	repos     []models.Repo
	ownerType string
	verifyErr error
	listErr   error
}

func (f fakeResolver) Verify(context.Context) (string, error) {
	return f.login, f.verifyErr
}

func (f fakeResolver) ListRepos(context.Context, string) ([]models.Repo, string, error) {
	return f.repos, f.ownerType, f.listErr
}

func TestRunSelectWritesLockfile(t *testing.T) {
	r := fakeResolver{
		login:     "redscaresu",
		ownerType: "User",
		repos: []models.Repo{
			{Owner: "redscaresu", Name: "platform-svc", Topics: []string{"platform"}},
			{Owner: "redscaresu", Name: "web", Topics: []string{"frontend"}},
			{Owner: "redscaresu", Name: "old", Topics: []string{"platform"}, Archived: true},
		},
	}
	path := filepath.Join(t.TempDir(), "goldfinger.selection")
	var out, errOut bytes.Buffer

	err := runSelect(context.Background(), r,
		targeting{org: "redscaresu", topics: []string{"platform"}},
		path, "goldfinger test", &out, &errOut)
	require.NoError(t, err)

	// Only the non-archived platform repo is selected.
	assert.Equal(t, "redscaresu/platform-svc\n", out.String())
	assert.Contains(t, errOut.String(), "1 repo(s) written")

	sel, err := selection.Read(path)
	require.NoError(t, err)
	assert.Equal(t, models.SelectionVersion, sel.Version)
	assert.Equal(t, "redscaresu", sel.Owner)
	assert.Equal(t, "User", sel.OwnerType)
	assert.Equal(t, []string{"platform"}, sel.Filter.Topics)
	assert.False(t, sel.ResolvedAt.IsZero())
	require.Len(t, sel.Repos, 1)
	assert.Equal(t, "redscaresu/platform-svc", sel.Repos[0].FullName())
}

func TestRunSelectPropagatesErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "goldfinger.selection")

	t.Run("verify error", func(t *testing.T) {
		err := runSelect(context.Background(),
			fakeResolver{verifyErr: errors.New("bad token")},
			targeting{org: "acme", allRepos: true}, path, "t", &bytes.Buffer{}, &bytes.Buffer{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "verifying token")
	})

	t.Run("list error", func(t *testing.T) {
		err := runSelect(context.Background(),
			fakeResolver{listErr: errors.New("not found")},
			targeting{org: "acme", allRepos: true}, path, "t", &bytes.Buffer{}, &bytes.Buffer{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})
}
