package client

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestClient points a real go-github client at a test server.
func newTestClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	base := srv.URL + "/"
	gh, err := github.NewClient(github.WithURLs(&base, nil))
	require.NoError(t, err)
	return &Client{gh: gh}
}

// repoJSON renders one repository object as the API would.
func repoJSON(owner, name string, topics []string, archived bool) string {
	quoted := make([]string, len(topics))
	for i, tp := range topics {
		quoted[i] = fmt.Sprintf("%q", tp)
	}
	return fmt.Sprintf(`{"name":%q,"owner":{"login":%q},"clone_url":"https://github.com/%s/%s.git","default_branch":"main","topics":[%s],"archived":%t}`,
		name, owner, owner, name, strings.Join(quoted, ","), archived)
}

func TestVerifyReturnsLogin(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"login":"octobot"}`)
	})
	c := newTestClient(t, mux)

	login, err := c.Verify(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "octobot", login)
}

func TestListReposOrgPagination(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/users/acme", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"login":"acme","type":"Organization"}`)
	})
	mux.HandleFunc("/orgs/acme/repos", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "", "1":
			w.Header().Set("Link", `<http://x?page=2>; rel="next"`)
			fmt.Fprintf(w, "[%s,%s]", repoJSON("acme", "one", []string{"platform"}, false), repoJSON("acme", "two", nil, false))
		case "2":
			w.Header().Set("Link", `<http://x?page=3>; rel="next"`)
			fmt.Fprintf(w, "[%s]", repoJSON("acme", "three", nil, true))
		default:
			fmt.Fprintf(w, "[%s]", repoJSON("acme", "four", []string{"go"}, false))
		}
	})
	c := newTestClient(t, mux)
	c.login = "someone-else" // skip the /user lookup; acme is not us

	repos, ownerType, err := c.ListRepos(context.Background(), "acme")
	require.NoError(t, err)
	assert.Equal(t, OwnerOrganization, ownerType)
	require.Len(t, repos, 4, "all three pages should be accumulated")

	names := []string{repos[0].Name, repos[1].Name, repos[2].Name, repos[3].Name}
	assert.Equal(t, []string{"one", "two", "three", "four"}, names)
	// client returns everything; filtering (archived, topics) is discovery's job.
	assert.True(t, repos[2].Archived)
	assert.Equal(t, []string{"platform"}, repos[0].Topics)
	assert.Equal(t, "https://github.com/acme/one.git", repos[0].CloneURL)
	assert.Equal(t, "main", repos[0].DefaultBranch)
}

func TestListReposUserPath(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/users/bob", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"login":"bob","type":"User"}`)
	})
	mux.HandleFunc("/users/bob/repos", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "[%s]", repoJSON("bob", "dotfiles", nil, false))
	})
	c := newTestClient(t, mux)
	c.login = "someone-else"

	repos, ownerType, err := c.ListRepos(context.Background(), "bob")
	require.NoError(t, err)
	assert.Equal(t, OwnerUser, ownerType)
	require.Len(t, repos, 1)
	assert.Equal(t, "bob/dotfiles", repos[0].FullName())
}

func TestListReposAuthenticatedOwnerPath(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"login":"me"}`)
	})
	mux.HandleFunc("/user/repos", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "owner", r.URL.Query().Get("affiliation"))
		fmt.Fprintf(w, "[%s]", repoJSON("me", "private-thing", nil, false))
	})
	c := newTestClient(t, mux)

	repos, ownerType, err := c.ListRepos(context.Background(), "me")
	require.NoError(t, err)
	assert.Equal(t, OwnerUser, ownerType)
	require.Len(t, repos, 1)
	assert.Equal(t, "me/private-thing", repos[0].FullName())
}

func TestListReposPropagatesError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/users/ghost", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	c := newTestClient(t, mux)
	c.login = "someone-else"

	_, _, err := c.ListRepos(context.Background(), "ghost")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ghost")
}
