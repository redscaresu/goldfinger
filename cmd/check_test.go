package main

import (
	"bytes"
	"context"
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

	err := runCheck(context.Background(), r, sel, &out, &errOut)
	require.NoError(t, err)
	assert.Empty(t, out.String(), "no drift report on stdout when in sync")
	assert.Contains(t, errOut.String(), "in sync with live discovery")
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

	err := runCheck(context.Background(), r, sel, &out, &errOut)

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

	err := runCheck(context.Background(), r, sel, &out, &errOut)

	var ee exitError
	require.ErrorAs(t, err, &ee)
	assert.Equal(t, 1, ee.code)
	assert.Contains(t, out.String(), "owner type changed: User -> Organization")
}

func TestRunCheckPropagatesErrors(t *testing.T) {
	sel := lockfile()

	t.Run("verify error", func(t *testing.T) {
		r := fakeResolver{verifyErr: errors.New("bad token")}
		err := runCheck(context.Background(), r, sel, &bytes.Buffer{}, &bytes.Buffer{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "verifying token")
		assert.Equal(t, 2, exitCode(err), "a real error exits 2, not 1")
	})

	t.Run("list error", func(t *testing.T) {
		r := fakeResolver{listErr: errors.New("not found")}
		err := runCheck(context.Background(), r, sel, &bytes.Buffer{}, &bytes.Buffer{})
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
