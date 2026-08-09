package apply

import (
	"testing"

	"github.com/redscaresu/goldfinger/models"
	"github.com/stretchr/testify/assert"
)

func TestSummarizeDryRunOutput(t *testing.T) {
	repos := []models.Repo{
		{Owner: "acme", Name: "api"},
		{Owner: "acme", Name: "worker"},
		{Owner: "acme", Name: "web"},
		{Owner: "acme", Name: "missing"},
	}

	tests := []struct {
		name string
		out  string
		want DryRunDigest
	}{
		{
			name: "mixed multi-gitter info buckets with logrus noise",
			out: `time="2026-08-09T10:00:00Z" level=info msg="Running on 4 repositories"
No data was changed:
time="2026-08-09T10:00:01Z" level=info msg="no data was changed"
  acme/worker
Script failed:
  acme/web
  someone/else
Repositories with a successful run:
  acme/api #0
time="2026-08-09T10:00:02Z" level=info msg="Skipping pushing changes because of dry run"
`,
			want: DryRunDigest{
				RepoCount: 4,
				Changed:   1,
				Unchanged: 1,
				Errored:   1,
				Repos: []RepoDryRunStatus{
					{Repo: "acme/api", Status: DryRunWouldChange},
					{Repo: "acme/worker", Status: DryRunNoChange},
					{Repo: "acme/web", Status: DryRunError, Detail: "Script failed"},
					{Repo: "acme/missing", Status: DryRunUnknown},
				},
			},
		},
		{
			name: "ansi headers and dry-run pull request suffix",
			out:  "\x1b[31mNo data was changed:\x1b[0m\n  acme/web\n\x1b[32mRepositories with a successful run:\x1b[0m\n  acme/api #0\n",
			want: DryRunDigest{
				RepoCount: 4,
				Changed:   1,
				Unchanged: 1,
				Errored:   0,
				Repos: []RepoDryRunStatus{
					{Repo: "acme/api", Status: DryRunWouldChange},
					{Repo: "acme/worker", Status: DryRunUnknown},
					{Repo: "acme/web", Status: DryRunNoChange},
					{Repo: "acme/missing", Status: DryRunUnknown},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, SummarizeDryRunOutput(repos, []byte(tt.out)))
		})
	}
}
