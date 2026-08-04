package discovery

import (
	"testing"

	"github.com/redscaresu/goldfinger/models"
	"github.com/stretchr/testify/assert"
)

func repo(name, branch string) models.Repo {
	return models.Repo{Owner: "acme", Name: name, DefaultBranch: branch}
}

func TestCompareInSync(t *testing.T) {
	locked := []models.Repo{repo("a", "main"), repo("b", "main")}
	live := []models.Repo{repo("b", "main"), repo("a", "main")} // order differs
	d := Compare(locked, live, live)
	assert.True(t, d.Empty())
}

func TestCompareAdded(t *testing.T) {
	locked := []models.Repo{repo("a", "main")}
	live := []models.Repo{repo("a", "main"), repo("b", "main"), repo("c", "main")}
	d := Compare(locked, live, live)

	assert.False(t, d.Empty())
	// Sorted by full name, deterministic regardless of map iteration.
	names := fullNames(d.Added)
	assert.Equal(t, []string{"acme/b", "acme/c"}, names)
	assert.Empty(t, d.Removed)
	assert.Empty(t, d.DefaultBranchMoved)
}

func TestCompareRemovedReasons(t *testing.T) {
	locked := []models.Repo{
		repo("archived", "main"),
		repo("unmatched", "main"),
		repo("gone", "main"),
	}
	// live (post-filter) has none of them.
	live := []models.Repo{}
	// raw (pre-filter) still exposes archived (as archived) and unmatched (not
	// archived, so it must have fallen out of the filter); "gone" is absent.
	raw := []models.Repo{
		{Owner: "acme", Name: "archived", Archived: true},
		{Owner: "acme", Name: "unmatched"},
	}
	d := Compare(locked, live, raw)

	assert.Len(t, d.Removed, 3)
	// Sorted by full name: archived, gone, unmatched.
	assert.Equal(t, "acme/archived", d.Removed[0].Repo.FullName())
	assert.Equal(t, "archived", d.Removed[0].Reason)
	assert.Equal(t, "acme/gone", d.Removed[1].Repo.FullName())
	assert.Equal(t, "deleted, transferred, or no longer visible to this token", d.Removed[1].Reason)
	assert.Equal(t, "acme/unmatched", d.Removed[2].Repo.FullName())
	assert.Equal(t, "no longer matches filter", d.Removed[2].Reason)
}

func TestCompareDefaultBranchMoved(t *testing.T) {
	locked := []models.Repo{repo("a", "main"), repo("b", "develop")}
	live := []models.Repo{repo("a", "develop"), repo("b", "develop")}
	d := Compare(locked, live, live)

	assert.Empty(t, d.Added)
	assert.Empty(t, d.Removed)
	assert.Len(t, d.DefaultBranchMoved, 1)
	assert.Equal(t, "acme/a", d.DefaultBranchMoved[0].Repo.FullName())
	assert.Equal(t, "main", d.DefaultBranchMoved[0].Was)
	assert.Equal(t, "develop", d.DefaultBranchMoved[0].Now)
}

func TestCompareBlankBranchIsNotDrift(t *testing.T) {
	// A lockfile that never recorded a default branch (older/hand-written) must
	// not report a spurious "" -> main move.
	locked := []models.Repo{repo("a", "")}
	live := []models.Repo{repo("a", "main")}
	d := Compare(locked, live, live)
	assert.True(t, d.Empty(), "blank recorded default should be treated as unknown, not drift")
}

func TestCompareCombination(t *testing.T) {
	locked := []models.Repo{repo("keep", "main"), repo("drop", "main"), repo("moved", "main")}
	live := []models.Repo{repo("keep", "main"), repo("moved", "develop"), repo("new", "main")}
	raw := append([]models.Repo{}, live...) // "drop" absent from raw -> gone
	d := Compare(locked, live, raw)

	assert.Equal(t, []string{"acme/new"}, fullNames(d.Added))
	assert.Len(t, d.Removed, 1)
	assert.Equal(t, "acme/drop", d.Removed[0].Repo.FullName())
	assert.Len(t, d.DefaultBranchMoved, 1)
	assert.Equal(t, "acme/moved", d.DefaultBranchMoved[0].Repo.FullName())
}

func fullNames(repos []models.Repo) []string {
	var out []string
	for _, r := range repos {
		out = append(out, r.FullName())
	}
	return out
}
