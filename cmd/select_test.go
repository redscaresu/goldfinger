package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/redscaresu/goldfinger/models"
	"github.com/redscaresu/goldfinger/selection"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeResolver struct {
	login        string
	repos        []models.Repo
	ownerType    string
	ownerTypeErr error
	verifyErr    error
	listErr      error

	// Branch-presence support for `select --branch-presence`.
	presentBranches map[string]bool // "owner/name@branch" -> exists
	branchErr       error
	branchCalls     *[]string // records each "owner/name@branch" probed

	// Explicit-selection support for `select --repo`/`--repos-from`.
	getRepos     map[string]models.Repo // basename -> repo; a missing key is a 404
	getRepoErr   error                  // forced error for any GetRepo call
	getRepoCalls *[]string              // records each "owner/name" looked up
}

func (f fakeResolver) Verify(context.Context) (string, error) {
	return f.login, f.verifyErr
}

func (f fakeResolver) ListRepos(context.Context, string) ([]models.Repo, string, error) {
	return f.repos, f.ownerType, f.listErr
}

func (f fakeResolver) OwnerType(context.Context, string) (string, error) {
	return f.ownerType, f.ownerTypeErr
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

func (f fakeResolver) GetRepo(_ context.Context, owner, name string) (models.Repo, string, error) {
	if f.getRepoCalls != nil {
		*f.getRepoCalls = append(*f.getRepoCalls, owner+"/"+name)
	}
	if f.getRepoErr != nil {
		return models.Repo{}, "", f.getRepoErr
	}
	repo, ok := f.getRepos[name]
	if !ok {
		return models.Repo{}, "", fmt.Errorf("repository %s/%s not found", owner, name)
	}
	return repo, f.ownerType, nil
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

	// Terse by default (issue #48 WS7): stdout stays empty — the list is in the
	// lockfile and the count is on the stderr done() line — so a large selection
	// doesn't dump one stdout line per repo. --list opts back into the names
	// (TestRunSelectListEchoesNames).
	assert.Empty(t, out.String(), "default stdout is terse; the count is on stderr, the list in the lockfile")
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

func TestRunSelectExplicitRepos(t *testing.T) {
	calls := []string{}
	r := fakeResolver{
		login:        "acme-bot",
		ownerType:    models.OwnerOrganization,
		getRepoCalls: &calls,
		getRepos: map[string]models.Repo{
			"svc-a": {Owner: "acme", Name: "svc-a", DefaultBranch: "main"},
			// An archived repo is deliberately kept in an explicit selection.
			"svc-b": {Owner: "acme", Name: "svc-b", DefaultBranch: "dev", Archived: true},
		},
	}
	path := filepath.Join(t.TempDir(), "goldfinger.selection")
	var out, errOut bytes.Buffer

	err := runSelect(context.Background(), r, selectOpts{
		t:             targeting{org: "acme", repos: []string{"svc-a", "svc-b"}},
		selectionPath: path,
		tool:          "goldfinger test",
		source:        tokenSourceEnv,
	}, &out, &errOut)
	require.NoError(t, err)

	// Explicit mode resolves each named repo directly — never a full owner listing.
	assert.Equal(t, []string{"acme/svc-a", "acme/svc-b"}, calls)

	sel, err := selection.Read(path)
	require.NoError(t, err)
	assert.Equal(t, models.OwnerOrganization, sel.OwnerType, "owner type comes from the per-repo GET")
	// Filter.Repos is the explicit-mode marker; the filter fields stay zero.
	assert.Equal(t, []string{"svc-a", "svc-b"}, sel.Filter.Repos)
	assert.False(t, sel.Filter.AllRepos)
	assert.Empty(t, sel.Filter.Topics)
	require.Len(t, sel.Repos, 2)
	assert.Equal(t, "acme/svc-a", sel.Repos[0].FullName())
	assert.True(t, sel.Repos[1].Archived, "an explicitly named archived repo is included")
}

func TestRunSelectExplicitRepoNotFoundIsHardError(t *testing.T) {
	r := fakeResolver{
		login:     "acme-bot",
		ownerType: models.OwnerOrganization,
		getRepos: map[string]models.Repo{
			"svc-a": {Owner: "acme", Name: "svc-a", DefaultBranch: "main"},
			// "typo" is intentionally absent -> GetRepo returns not-found.
		},
	}
	path := filepath.Join(t.TempDir(), "goldfinger.selection")
	var out, errOut bytes.Buffer

	err := runSelect(context.Background(), r, selectOpts{
		t:             targeting{org: "acme", repos: []string{"svc-a", "typo"}},
		selectionPath: path,
		tool:          "goldfinger test",
		source:        tokenSourceEnv,
	}, &out, &errOut)
	require.Error(t, err, "a named repo that 404s must fail loudly, not be dropped")
	assert.Contains(t, err.Error(), "acme/typo")
}

func TestRunSelectExplicitEmptyObeysAllowEmpty(t *testing.T) {
	// reposFrom is set (flipping explicit mode on) but repos resolved to none,
	// e.g. an empty or all-comments --repos-from file.
	newResolver := func() fakeResolver {
		return fakeResolver{login: "acme-bot", ownerType: models.OwnerOrganization}
	}
	base := selectOpts{
		t:             targeting{org: "acme", reposFrom: "repos.txt"},
		tool:          "goldfinger test",
		source:        tokenSourceEnv,
	}

	t.Run("empty explicit set is an error by default", func(t *testing.T) {
		o := base
		o.selectionPath = filepath.Join(t.TempDir(), "goldfinger.selection")
		var out, errOut bytes.Buffer
		err := runSelect(context.Background(), newResolver(), o, &out, &errOut)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--allow-empty")
	})

	t.Run("--allow-empty writes an empty explicit lockfile with a valid ownerType", func(t *testing.T) {
		o := base
		o.allowEmpty = true
		o.selectionPath = filepath.Join(t.TempDir(), "goldfinger.selection")
		var out, errOut bytes.Buffer
		err := runSelect(context.Background(), newResolver(), o, &out, &errOut)
		require.NoError(t, err)
		sel, err := selection.Read(o.selectionPath)
		require.NoError(t, err)
		assert.Empty(t, sel.Repos)
		// With no repo to read the type from, ownerType is probed directly so the
		// lockfile stays schema-valid (ownerType is an enum, not "").
		assert.Equal(t, models.OwnerOrganization, sel.OwnerType)
	})
}

// TestRunSelectExplicitRejectsRedirectedRepo locks the single-owner invariant
// against GitHub's rename/transfer redirect: GetRepo can return a repo under a
// different owner or name than requested, and freezing that would make
// Selection.Owner disagree with a repo's real owner. Such a mismatch is a hard
// error, not a silent acceptance.
func TestRunSelectExplicitRejectsRedirectedRepo(t *testing.T) {
	run := func(t *testing.T, got models.Repo) error {
		t.Helper()
		r := fakeResolver{
			login:     "acme-bot",
			ownerType: models.OwnerOrganization,
			getRepos:  map[string]models.Repo{"svc": got},
		}
		return runSelect(context.Background(), r, selectOpts{
			t:             targeting{org: "acme", repos: []string{"svc"}},
			selectionPath: filepath.Join(t.TempDir(), "goldfinger.selection"),
			tool:          "goldfinger test",
			source:        tokenSourceEnv,
		}, &bytes.Buffer{}, &bytes.Buffer{})
	}

	t.Run("transfer to a different owner is rejected", func(t *testing.T) {
		err := run(t, models.Repo{Owner: "other", Name: "svc", DefaultBranch: "main"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "other/svc")
		assert.Contains(t, err.Error(), "renamed or transferred")
	})

	t.Run("rename to a different name is rejected", func(t *testing.T) {
		err := run(t, models.Repo{Owner: "acme", Name: "svc-renamed", DefaultBranch: "main"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "acme/svc-renamed")
	})

	t.Run("a case-only difference is accepted (GitHub names are case-insensitive)", func(t *testing.T) {
		err := run(t, models.Repo{Owner: "Acme", Name: "SVC", DefaultBranch: "main"})
		require.NoError(t, err, "same repo in different casing is not a redirect")
	})
}

func TestResolveTargetRepos(t *testing.T) {
	t.Run("merges, normalises owner/name, and dedupes", func(t *testing.T) {
		got, err := resolveTargetRepos(targeting{
			org:   "acme",
			repos: []string{"svc-a", "acme/svc-b", "svc-a"}, // owner-prefixed + duplicate
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"svc-a", "svc-b"}, got)
	})

	t.Run("a different owner is a hard error", func(t *testing.T) {
		_, err := resolveTargetRepos(targeting{org: "acme", repos: []string{"other/svc"}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "other")
		assert.Contains(t, err.Error(), "--org")
	})

	t.Run("owner prefix matches --org case-insensitively", func(t *testing.T) {
		got, err := resolveTargetRepos(targeting{org: "acme", repos: []string{"ACME/svc-a"}})
		require.NoError(t, err)
		assert.Equal(t, []string{"svc-a"}, got, "GitHub owner logins are case-insensitive")
	})

	t.Run("case-only duplicate names collapse to first-seen", func(t *testing.T) {
		got, err := resolveTargetRepos(targeting{org: "acme", repos: []string{"Svc", "svc", "acme/SVC"}})
		require.NoError(t, err)
		assert.Equal(t, []string{"Svc"}, got, "one repo (GitHub names are case-insensitive), first casing kept")
	})

	t.Run("reads --repos-from, ignoring blanks and comments", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "repos.txt")
		require.NoError(t, os.WriteFile(file, []byte("# a comment\nsvc-a\n\n  svc-b  \n# trailing\n"), 0o600))
		got, err := resolveTargetRepos(targeting{org: "acme", reposFrom: file})
		require.NoError(t, err)
		assert.Equal(t, []string{"svc-a", "svc-b"}, got)
	})

	t.Run("a missing --repos-from file errors", func(t *testing.T) {
		_, err := resolveTargetRepos(targeting{org: "acme", reposFrom: filepath.Join(t.TempDir(), "nope.txt")})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "repos-from")
	})
}

func TestRunSelectListEchoesNames(t *testing.T) {
	r := fakeResolver{
		login:     "redscaresu",
		ownerType: models.OwnerUser,
		repos: []models.Repo{
			{Owner: "redscaresu", Name: "platform-svc", Topics: []string{"platform"}},
			{Owner: "redscaresu", Name: "platform-api", Topics: []string{"platform"}},
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
		list:          true,
	}, &out, &errOut)
	require.NoError(t, err)

	// --list restores the full-name echo on stdout, one per selected repo.
	assert.Equal(t, "redscaresu/platform-svc\nredscaresu/platform-api\n", out.String())
	assert.Contains(t, errOut.String(), "2 repo(s) written")
}

// TestRunSelectListBeatsQuietPath locks the output precedence: --list wins over
// quiet's path-only stdout, so `select --quiet --list` echoes the names (not the
// lockfile path). This pins the documented switch order (json > list > quiet).
func TestRunSelectListBeatsQuietPath(t *testing.T) {
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
		list:          true,
		quiet:         true,
	}, &out, &errOut)
	require.NoError(t, err)

	assert.Equal(t, "redscaresu/platform-svc\n", out.String(), "--list echoes names even under --quiet")
	assert.NotContains(t, out.String(), path, "the lockfile path is not printed when --list wins")
	assert.Empty(t, errOut.String(), "quiet still suppresses stderr banners/done line")
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

	// The wrapper carries the repo-set digest (issue #48 WS6): a machine consumer
	// gets the fingerprint alongside the selection without recomputing it, and it
	// matches selection.Digest over the same repos.
	_, wantDigest := selection.Digest(onDisk)
	assert.Equal(t, wantDigest, rep.Digest)
	assert.NotEmpty(t, rep.Digest)

	// Human banners stay on stderr, and the digest rides that line too.
	assert.Contains(t, errOut.String(), "1 repo(s) written")
	assert.Contains(t, errOut.String(), "digest "+wantDigest)
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
