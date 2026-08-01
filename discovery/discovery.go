// Package discovery turns targeting flags into a filtered set of repos. It is
// pure logic over plain structs — no network access — so it is trivial to test.
package discovery

import "github.com/redscaresu/goldfinger/models"

// Filter expresses which repos a run should target.
type Filter struct {
	AllRepos bool
	Topics   []string // match a repo carrying ANY of these topics
}

// Select returns the repos matching f. Archived repos are always excluded.
func Select(repos []models.Repo, f Filter) []models.Repo {
	var out []models.Repo
	for _, r := range repos {
		if r.Archived {
			continue
		}
		if f.AllRepos || hasAnyTopic(r.Topics, f.Topics) {
			out = append(out, r)
		}
	}
	return out
}

func hasAnyTopic(repoTopics, want []string) bool {
	if len(want) == 0 {
		return false
	}
	have := make(map[string]struct{}, len(repoTopics))
	for _, t := range repoTopics {
		have[t] = struct{}{}
	}
	for _, w := range want {
		if _, ok := have[w]; ok {
			return true
		}
	}
	return false
}
