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

	// Branch-presence support for `select --branch-presence`.
	presentBranches map[string]bool // "owner/name@branch" -> exists
	branchErr       error
	branchCalls     *[]string // records each "owner/name@branch" probed
}

func (f fakeResolver) Verify(context.Context) (string, error) {
	return f.login, f.verifyErr
}

func (f fakeResolver) ListRepos(context.Context, string) ([]models.Repo, string, error) {
	return f.repos, f.ownerType, f.listErr
}

func (f fakeResolver) BranchExists(_ context.Context, owner, repo, branch string) (bool, error) {
	key := owner + "/" + repo + "@" + branch
	if f.branchCalls != nil {
		*f.branchCalls = append(*f.branchCalls, key)
	}
	if f.branchErr != nil {
		return false, f.branchErr
	}
	return f.presentBranches[key], nil
}

func TestRunSelectWritesLockfile(t *testing.T) {
	r := fakeResolver{
		login:     "redscaresu",
		ownerType: models.OwnerUser,
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
		nil, path, "goldfinger test", &out, &errOut)
	require.NoError(t, err)

	// Only the non-archived platform repo is selected.
	assert.Equal(t, "redscaresu/platform-svc\n", out.String())
	assert.Contains(t, errOut.String(), "1 repo(s) written")

	sel, err := selection.Read(path)
	require.NoError(t, err)
	assert.Equal(t, models.SelectionVersion, sel.Version)
	assert.Equal(t, "redscaresu", sel.Owner)
	assert.Equal(t, models.OwnerUser, sel.OwnerType)
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
			targeting{org: "acme", allRepos: true}, nil, path, "t", &bytes.Buffer{}, &bytes.Buffer{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "verifying token")
	})

	t.Run("list error", func(t *testing.T) {
		err := runSelect(context.Background(),
			fakeResolver{listErr: errors.New("not found")},
			targeting{org: "acme", allRepos: true}, nil, path, "t", &bytes.Buffer{}, &bytes.Buffer{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})
}

func TestRunSelectRecordsBranchPresence(t *testing.T) {
	var calls []string
	r := fakeResolver{
		login:     "redscaresu",
		ownerType: models.OwnerUser,
		repos: []models.Repo{
			{Owner: "redscaresu", Name: "on-dev", DefaultBranch: "main", Topics: []string{"platform"}},
			{Owner: "redscaresu", Name: "default-is-dev", DefaultBranch: "dev", Topics: []string{"platform"}},
			{Owner: "redscaresu", Name: "no-dev", DefaultBranch: "main", Topics: []string{"platform"}},
			{Owner: "redscaresu", Name: "archived", DefaultBranch: "main", Topics: []string{"platform"}, Archived: true},
		},
		presentBranches: map[string]bool{
			"redscaresu/on-dev@dev": true,
			// no-dev has no "dev" entry -> BranchExists returns false
		},
		branchCalls: &calls,
	}
	path := filepath.Join(t.TempDir(), "goldfinger.selection")

	// Duplicate --branch-presence dev must be deduped.
	err := runSelect(context.Background(), r,
		targeting{org: "redscaresu", topics: []string{"platform"}},
		[]string{"dev", "dev"}, path, "goldfinger test", &bytes.Buffer{}, &bytes.Buffer{})
	require.NoError(t, err)

	// dev probed once each for on-dev and no-dev only: the archived repo is not in
	// the selection, and default-is-dev short-circuits (dev is its default).
	assert.ElementsMatch(t, []string{"redscaresu/on-dev@dev", "redscaresu/no-dev@dev"}, calls)

	sel, err := selection.Read(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"dev"}, sel.BranchesChecked, "dedup: dev recorded once")
	require.Len(t, sel.Repos, 3, "archived repo excluded from selection")

	byName := map[string]models.Repo{}
	for _, repo := range sel.Repos {
		byName[repo.Name] = repo
	}

	has, known := byName["on-dev"].RecordedBranch("dev")
	assert.True(t, known)
	assert.True(t, has)

	has, known = byName["no-dev"].RecordedBranch("dev")
	assert.True(t, known)
	assert.False(t, has)

	// default-is-dev: present-by-definition, recorded without an API call.
	has, known = byName["default-is-dev"].RecordedBranch("dev")
	assert.True(t, known)
	assert.True(t, has)
}
