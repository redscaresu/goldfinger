package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/redscaresu/goldfinger/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lockfile builds a two-repo User selection resolved over --all-repos, the
// baseline the drift tests perturb.
func lockfile() models.Selection {
	return models.Selection{
		Version:   models.SelectionVersion,
		Owner:     "acme",
		OwnerType: models.OwnerUser,
		Filter:    models.SelectionFilter{AllRepos: true},
		Repos: []models.Repo{
			{Owner: "acme", Name: "a", DefaultBranch: "main"},
			{Owner: "acme", Name: "b", DefaultBranch: "main"},
		},
	}
}

func TestRunCheckInSync(t *testing.T) {
	sel := lockfile()
	r := fakeResolver{ownerType: models.OwnerUser, repos: sel.Repos}
	var out, errOut bytes.Buffer

	err := runCheck(context.Background(), r, sel, checkOpts{}, &out, &errOut)
	require.NoError(t, err)
	assert.Empty(t, out.String(), "no drift report on stdout when in sync")
	assert.Contains(t, errOut.String(), "in sync with live discovery")
}

func TestRunCheckJSON(t *testing.T) {
	t.Run("in sync emits report and exits 0", func(t *testing.T) {
		sel := lockfile()
		r := fakeResolver{ownerType: models.OwnerUser, repos: sel.Repos}
		var out, errOut bytes.Buffer

		err := runCheck(context.Background(), r, sel, checkOpts{name: "platform", asJSON: true}, &out, &errOut)
		require.NoError(t, err)

		var rep checkReport
		require.NoError(t, json.Unmarshal(out.Bytes(), &rep))
		assert.Equal(t, checkReportVersion, rep.Version)
		assert.Equal(t, "platform", rep.Name)
		assert.True(t, rep.InSync)
		assert.Empty(t, rep.Added)
		assert.Empty(t, rep.Removed)
		assert.Nil(t, rep.OwnerTypeFlipped, "nullable object is null when unchanged")
		// stdout is JSON only.
		assert.NotContains(t, errOut.String(), "{")
	})

	t.Run("drift emits report and exits 1", func(t *testing.T) {
		sel := lockfile()
		r := fakeResolver{
			ownerType: models.OwnerOrganization, // owner type also flipped
			repos: []models.Repo{
				{Owner: "acme", Name: "a", DefaultBranch: "dev"}, // default branch moved
				{Owner: "acme", Name: "c", DefaultBranch: "main"},
			},
		}
		var out, errOut bytes.Buffer

		err := runCheck(context.Background(), r, sel, checkOpts{asJSON: true}, &out, &errOut)
		var ee exitError
		require.ErrorAs(t, err, &ee)
		assert.Equal(t, 1, ee.code)

		var rep checkReport
		require.NoError(t, json.Unmarshal(out.Bytes(), &rep))
		assert.False(t, rep.InSync)
		assert.Empty(t, rep.Name, "name omitted for a default selection")
		assert.Equal(t, []string{"acme/c"}, rep.Added)
		require.Len(t, rep.Removed, 1)
		assert.Equal(t, "acme/b", rep.Removed[0].Repo)
		require.Len(t, rep.DefaultBranchMoved, 1)
		assert.Equal(t, "acme/a", rep.DefaultBranchMoved[0].Repo)
		assert.Equal(t, "main", rep.DefaultBranchMoved[0].From)
		assert.Equal(t, "dev", rep.DefaultBranchMoved[0].To)
		require.NotNil(t, rep.OwnerTypeFlipped)
		assert.Equal(t, models.OwnerUser, rep.OwnerTypeFlipped.From)
		assert.Equal(t, models.OwnerOrganization, rep.OwnerTypeFlipped.To)
	})
}

func TestRunCheckReportsRepoDrift(t *testing.T) {
	sel := lockfile()
	// Live: "a" unchanged, "b" gone, "c" added.
	r := fakeResolver{
		ownerType: models.OwnerUser,
		repos: []models.Repo{
			{Owner: "acme", Name: "a", DefaultBranch: "main"},
			{Owner: "acme", Name: "c", DefaultBranch: "main"},
		},
	}
	var out, errOut bytes.Buffer

	err := runCheck(context.Background(), r, sel, checkOpts{}, &out, &errOut)

	var ee exitError
	require.ErrorAs(t, err, &ee)
	assert.Equal(t, 1, ee.code, "drift exits 1")

	s := out.String()
	assert.Contains(t, s, "+ acme/c")
	assert.Contains(t, s, "- acme/b")
	assert.Contains(t, s, "deleted, transferred, or no longer visible")
	assert.Contains(t, s, "summary: 1 unchanged, 1 added, 1 removed, 0 branch moved")
}

func TestRunCheckReportsOwnerTypeDrift(t *testing.T) {
	sel := lockfile() // recorded as User
	// Same repos, but the owner is now an Organization.
	r := fakeResolver{ownerType: models.OwnerOrganization, repos: sel.Repos}
	var out, errOut bytes.Buffer

	err := runCheck(context.Background(), r, sel, checkOpts{}, &out, &errOut)

	var ee exitError
	require.ErrorAs(t, err, &ee)
	assert.Equal(t, 1, ee.code)
	assert.Contains(t, out.String(), "owner type changed: User -> Organization")
}

func TestRunCheckPropagatesErrors(t *testing.T) {
	sel := lockfile()

	t.Run("verify error", func(t *testing.T) {
		r := fakeResolver{verifyErr: errors.New("bad token")}
		err := runCheck(context.Background(), r, sel, checkOpts{}, &bytes.Buffer{}, &bytes.Buffer{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "verifying token")
		assert.Equal(t, 2, exitCode(err), "a real error exits 2, not 1")
	})

	t.Run("list error", func(t *testing.T) {
		r := fakeResolver{listErr: errors.New("not found")}
		err := runCheck(context.Background(), r, sel, checkOpts{}, &bytes.Buffer{}, &bytes.Buffer{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})
}

func TestExitCode(t *testing.T) {
	assert.Equal(t, 0, exitCode(nil))
	assert.Equal(t, 1, exitCode(exitError{code: 1}))
	assert.Equal(t, 2, exitCode(errors.New("boom")))
	// A wrapped exitError is still recognised (errors.As unwraps).
	assert.Equal(t, 1, exitCode(fmt.Errorf("context: %w", exitError{code: 1})))
}
