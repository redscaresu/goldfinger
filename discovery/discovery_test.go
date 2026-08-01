package discovery

import (
	"testing"

	"github.com/redscaresu/goldfinger/models"
	"github.com/stretchr/testify/assert"
)

func TestSelect(t *testing.T) {
	repos := []models.Repo{
		{Owner: "acme", Name: "platform-svc", Topics: []string{"platform", "go"}},
		{Owner: "acme", Name: "web", Topics: []string{"frontend"}},
		{Owner: "acme", Name: "legacy", Topics: []string{"platform"}, Archived: true},
		{Owner: "acme", Name: "untagged"},
	}

	tests := []struct {
		name   string
		filter Filter
		want   []string // repo names, in order
	}{
		{
			name:   "all-repos excludes archived",
			filter: Filter{AllRepos: true},
			want:   []string{"platform-svc", "web", "untagged"},
		},
		{
			name:   "single topic",
			filter: Filter{Topics: []string{"platform"}},
			want:   []string{"platform-svc"}, // legacy also has it but is archived
		},
		{
			name:   "multiple topics match any",
			filter: Filter{Topics: []string{"frontend", "go"}},
			want:   []string{"platform-svc", "web"},
		},
		{
			name:   "topic with no matches",
			filter: Filter{Topics: []string{"rust"}},
			want:   nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Select(repos, tt.filter)
			var names []string
			for _, r := range got {
				names = append(names, r.Name)
			}
			assert.Equal(t, tt.want, names)
		})
	}
}
