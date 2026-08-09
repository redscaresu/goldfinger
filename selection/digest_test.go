package selection

import (
	"testing"

	"github.com/redscaresu/goldfinger/models"
	"github.com/stretchr/testify/assert"
)

func repoSet(names ...string) []models.Repo {
	repos := make([]models.Repo, len(names))
	for i, n := range names {
		// names are "owner/name"; split on the single slash.
		for j := 0; j < len(n); j++ {
			if n[j] == '/' {
				repos[i] = models.Repo{Owner: n[:j], Name: n[j+1:]}
				break
			}
		}
	}
	return repos
}

func TestDigest(t *testing.T) {
	t.Run("count is the repo count; hash is the short fingerprint", func(t *testing.T) {
		count, hash := Digest(models.Selection{Repos: repoSet("acme/a", "acme/b", "acme/c")})
		assert.Equal(t, 3, count)
		assert.Len(t, hash, digestHashLen)
	})

	t.Run("order-independent: same set in any order hashes the same", func(t *testing.T) {
		_, h1 := Digest(models.Selection{Repos: repoSet("acme/a", "acme/b", "acme/c")})
		_, h2 := Digest(models.Selection{Repos: repoSet("acme/c", "acme/a", "acme/b")})
		assert.Equal(t, h1, h2, "the digest fingerprints the SET, not the discovery order")
	})

	t.Run("different set hashes differently", func(t *testing.T) {
		_, h1 := Digest(models.Selection{Repos: repoSet("acme/a", "acme/b")})
		_, h2 := Digest(models.Selection{Repos: repoSet("acme/a", "acme/c")})
		assert.NotEqual(t, h1, h2)
	})

	t.Run("empty selection has a stable zero-repo digest", func(t *testing.T) {
		count, hash := Digest(models.Selection{})
		assert.Equal(t, 0, count)
		assert.Len(t, hash, digestHashLen)
		// A branch-presence or provenance difference must not change the fingerprint
		// of the same (empty) repo set.
		_, hash2 := Digest(models.Selection{Owner: "acme", Tool: "goldfinger x"})
		assert.Equal(t, hash, hash2)
	})
}
