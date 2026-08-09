package main

import (
	"bytes"
	"context"
	"encoding/json"
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

	err := runSelect(context.Background(), r, selectOpts{
		t:             targeting{org: "redscaresu", topics: []string{"platform"}},
		selectionPath: path,
		tool:          "goldfinger test",
		source:        tokenSourceEnv,
	}, &out, &errOut)
	require.NoError(t, err)

	// Only the non-archived platform repo is selected.
	assert.Equal(t, "redscaresu/platform-svc\n", out.String())
	assert.Contains(t, errOut.String(), "1 repo(s) written")
	// The resolved identity is surfaced on stderr for wrong-token diagnosis.
	assert.Contains(t, errOut.String(), "authenticated as redscaresu")

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

func TestRunSelectJSON(t *testing.T) {
	r := fakeResolver{
		login:     "redscaresu",
		ownerType: models.OwnerUser,
		repos: []models.Repo{
			{Owner: "redscaresu", Name: "platform-svc", DefaultBranch: "main", Topics: []string{"platform"}},
		},
	}
	path := filepath.Join(t.TempDir(), "goldfinger.selection")
	var out, errOut bytes.Buffer

	err := runSelect(context.Background(), r, selectOpts{
		t:             targeting{org: "redscaresu", topics: []string{"platform"}},
		selectionPath: path,
		tool:          "goldfinger test",
		source:        tokenSourceEnv,
		asJSON:        true,
	}, &out, &errOut)
	require.NoError(t, err)

	// stdout is exactly the JSON wrapper — no repo-name lines leak in.
	var rep selectJSONReport
	require.NoError(t, json.Unmarshal(out.Bytes(), &rep))
	assert.Equal(t, path, rep.SelectionPath)
	assert.Equal(t, models.SelectionVersion, rep.Selection.Version, "nested selection carries the lockfile version")
	require.Len(t, rep.Selection.Repos, 1)
	assert.Equal(t, "redscaresu/platform-svc", rep.Selection.Repos[0].FullName())

	// The nested selection is field-for-field the persisted lockfile.
	onDisk, err := selection.Read(path)
	require.NoError(t, err)
	assert.Equal(t, onDisk.Owner, rep.Selection.Owner)
	assert.Equal(t, onDisk.Repos, rep.Selection.Repos)

	// Human banners stay on stderr.
	assert.Contains(t, errOut.String(), "1 repo(s) written")
	assert.NotContains(t, out.String(), "written to")
}

func TestRunSelectQuietPrintsLockfilePathOnly(t *testing.T) {
	r := fakeResolver{
		login:     "redscaresu",
		ownerType: models.OwnerUser,
		repos: []models.Repo{
			{Owner: "redscaresu", Name: "platform-svc", Topics: []string{"platform"}},
			{Owner: "redscaresu", Name: "web", Topics: []string{"frontend"}},
		},
	}
	path := filepath.Join(t.TempDir(), "goldfinger.selection")
	var out, errOut bytes.Buffer

	err := runSelect(context.Background(), r, selectOpts{
		t:             targeting{org: "redscaresu", topics: []string{"platform"}},
		selectionPath: path,
		tool:          "goldfinger test",
		source:        tokenSourceEnv,
		quiet:         true,
	}, &out, &errOut)
	require.NoError(t, err)

	assert.Equal(t, path+"\n", out.String())
	assert.NotContains(t, out.String(), "redscaresu/platform-svc")
	assert.Empty(t, errOut.String(), "quiet suppresses banners/auth/done lines")
}

func TestRunSelectQuietJSONPrintsJSONOnly(t *testing.T) {
	r := fakeResolver{
		login:     "redscaresu",
		ownerType: models.OwnerUser,
		repos:     []models.Repo{{Owner: "redscaresu", Name: "platform-svc", Topics: []string{"platform"}}},
	}
	path := filepath.Join(t.TempDir(), "goldfinger.selection")
	var out, errOut bytes.Buffer

	err := runSelect(context.Background(), r, selectOpts{
		t:             targeting{org: "redscaresu", topics: []string{"platform"}},
		selectionPath: path,
		tool:          "goldfinger test",
		source:        tokenSourceEnv,
		asJSON:        true,
		quiet:         true,
	}, &out, &errOut)
	require.NoError(t, err)

	var rep selectJSONReport
	require.NoError(t, json.Unmarshal(out.Bytes(), &rep))
	assert.Equal(t, path, rep.SelectionPath)
	assert.Empty(t, errOut.String(), "quiet suppresses human stderr")
	assert.True(t, bytes.HasPrefix(bytes.TrimSpace(out.Bytes()), []byte("{")), "stdout must be one JSON document, not a path plus JSON")
}

func TestRunSelectPropagatesErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "goldfinger.selection")

	t.Run("verify error", func(t *testing.T) {
		err := runSelect(context.Background(),
			fakeResolver{verifyErr: errors.New("bad token")},
			selectOpts{t: targeting{org: "acme", allRepos: true}, selectionPath: path, tool: "t"},
			&bytes.Buffer{}, &bytes.Buffer{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "verifying token")
	})

	t.Run("list error", func(t *testing.T) {
		err := runSelect(context.Background(),
			fakeResolver{listErr: errors.New("not found")},
			selectOpts{t: targeting{org: "acme", allRepos: true}, selectionPath: path, tool: "t"},
			&bytes.Buffer{}, &bytes.Buffer{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})
}

func TestRunSelectEmptyResult(t *testing.T) {
	// A valid token whose ListRepos yields repos that match no topic -> zero
	// selected. Without --allow-empty this is an error and no lockfile is written.
	r := fakeResolver{
		login:     "someone-else",
		ownerType: models.OwnerUser,
		repos: []models.Repo{
			{Owner: "acme", Name: "web", Topics: []string{"frontend"}},
		},
	}

	t.Run("errors and does not write", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "goldfinger.selection")
		err := runSelect(context.Background(), r, selectOpts{
			t:             targeting{org: "acme", topics: []string{"platform"}},
			selectionPath: path,
			tool:          "t",
			source:        tokenSourceGh,
		}, &bytes.Buffer{}, &bytes.Buffer{})
		require.Error(t, err)
		// The diagnostic names the identity and inputs so a wrong token is obvious.
		assert.Contains(t, err.Error(), "no repositories matched")
		assert.Contains(t, err.Error(), "someone-else")
		assert.Contains(t, err.Error(), "--allow-empty")
		_, statErr := selection.Read(path)
		require.Error(t, statErr, "no lockfile should be written on an empty result")
	})

	t.Run("allow-empty writes an empty lockfile", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "goldfinger.selection")
		err := runSelect(context.Background(), r, selectOpts{
			t:             targeting{org: "acme", topics: []string{"platform"}},
			selectionPath: path,
			tool:          "t",
			source:        tokenSourceGh,
			allowEmpty:    true,
		}, &bytes.Buffer{}, &bytes.Buffer{})
		require.NoError(t, err)
		sel, err := selection.Read(path)
		require.NoError(t, err)
		assert.Empty(t, sel.Repos)
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
	err := runSelect(context.Background(), r, selectOpts{
		t:               targeting{org: "redscaresu", topics: []string{"platform"}},
		branchesToCheck: []string{"dev", "dev"},
		selectionPath:   path,
		tool:            "goldfinger test",
	}, &bytes.Buffer{}, &bytes.Buffer{})
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
