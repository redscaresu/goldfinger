package client

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-github/v89/github"
	"github.com/redscaresu/goldfinger/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestClient points a real go-github client at a test server, wired exactly
// as New wires the production one.
func newTestClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	return newTestClientWithTimeout(t, h, requestTimeout)
}

// newTestClientWithTimeout is newTestClient with the request bound shortened, so
// a test can provoke a timeout without waiting for the production one.
func newTestClientWithTimeout(t *testing.T, h http.Handler, timeout time.Duration) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	base := srv.URL + "/"
	c, err := newClient("test-token", timeout, github.WithURLs(&base, nil))
	require.NoError(t, err)
	return c
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

// Every exported call, not just a representative one. go-github dispatches some
// methods through http.Client.Do and others straight at the transport, and only
// the first kind honours http.Client.Timeout — a difference invisible from the
// call site, which is how the highest-volume call (BranchExists, via
// `select --branch-presence`) came to be the one left unbounded. So the bound is
// asserted per method: a new method reaching for another bypassing helper fails
// here rather than shipping an unbounded request nobody thought to check.
func TestEveryCallIsBoundedWhenTheServerNeverAnswers(t *testing.T) {
	cases := []struct {
		name string
		// login pre-resolves the authenticated user, so a call that would
		// otherwise stall on /user inside ensureLogin stalls on its own
		// endpoint instead. Verify leaves it empty — /user is its endpoint.
		login string
		// respond answers the lookups a call makes on its way to the request
		// under test, so the row stalls on the one it is named for rather than
		// on a prerequisite. Everything else stalls.
		respond map[string]string
		// stallsOn is the endpoint the row must actually reach, asserted so a
		// row cannot quietly degrade into re-testing a prerequisite: before
		// this was checked, the ListRepos row stalled inside the owner lookup
		// and never issued a listing request at all.
		stallsOn string
		call     func(context.Context, *Client) error
	}{
		{name: "Verify", stallsOn: "/user", call: func(ctx context.Context, c *Client) error {
			_, err := c.Verify(ctx)
			return err
		}},
		// The three dispatches of ListRepos are separate go-github methods, so
		// each is its own row: a bypassing helper could appear in any one of
		// them. Own repos short-circuits the owner lookup by matching login.
		{name: "ListRepos/ownRepos", login: "acme", stallsOn: "/user/repos", call: func(ctx context.Context, c *Client) error {
			_, _, err := c.ListRepos(ctx, "acme")
			return err
		}},
		{
			name:     "ListRepos/org",
			login:    "octobot",
			respond:  map[string]string{"/users/acme": `{"login":"acme","type":"Organization"}`},
			stallsOn: "/orgs/acme/repos",
			call: func(ctx context.Context, c *Client) error {
				_, _, err := c.ListRepos(ctx, "acme")
				return err
			},
		},
		{
			name:     "ListRepos/user",
			login:    "octobot",
			respond:  map[string]string{"/users/acme": `{"login":"acme","type":"User"}`},
			stallsOn: "/users/acme/repos",
			call: func(ctx context.Context, c *Client) error {
				_, _, err := c.ListRepos(ctx, "acme")
				return err
			},
		},
		{name: "OwnerType", login: "octobot", stallsOn: "/users/acme", call: func(ctx context.Context, c *Client) error {
			_, err := c.OwnerType(ctx, "acme")
			return err
		}},
		{name: "GetRepo", login: "octobot", stallsOn: "/repos/acme/one", call: func(ctx context.Context, c *Client) error {
			_, _, err := c.GetRepo(ctx, "acme", "one")
			return err
		}},
		{name: "BranchExists", login: "octobot", stallsOn: "/repos/acme/one/branches/dev", call: func(ctx context.Context, c *Client) error {
			_, err := c.BranchExists(ctx, "acme", "one", "dev")
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			for path, body := range tc.respond {
				mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
					fmt.Fprint(w, body)
				})
			}
			var mu sync.Mutex
			var stalled []string
			// Everything not answered above stalls: the server accepts the
			// request and then never replies, which is the failure a timeout
			// exists for, as distinct from a refused connection. It releases
			// when the client hangs up, and on a timer regardless, so a
			// regression here fails slowly instead of deadlocking the test
			// server's shutdown (httptest.Server.Close waits for handlers).
			mux.HandleFunc("/", func(_ http.ResponseWriter, r *http.Request) {
				mu.Lock()
				stalled = append(stalled, r.URL.Path)
				mu.Unlock()
				select {
				case <-r.Context().Done():
				case <-time.After(30 * time.Second):
				}
			})
			c := newTestClientWithTimeout(t, mux, 100*time.Millisecond)
			c.login = tc.login

			start := time.Now()
			err := tc.call(context.Background(), c)

			require.Error(t, err, "a request that never gets a response must fail, not hang")
			assert.Less(t, time.Since(start), 10*time.Second,
				"the request should have been abandoned at its deadline rather than left to stall")

			// Eventually, not an immediate read: the client gives up 100ms in,
			// which on a loaded runner can be before the server's handler
			// goroutine has recorded the path it is already blocked on. Waiting
			// for the record costs nothing when it is already there and removes
			// the only flake vector here, without weakening the assertion.
			require.Eventually(t, func() bool {
				mu.Lock()
				defer mu.Unlock()
				return len(stalled) > 0
			}, 5*time.Second, 10*time.Millisecond, "the call never reached the server at all")

			mu.Lock()
			defer mu.Unlock()
			assert.Equal(t, []string{tc.stallsOn}, stalled,
				"this row must reach and stall on the request it is named for, not on a prerequisite")
		})
	}
}

// The deadline has to outlive the round trip that created it. RoundTrip returns
// as soon as the response headers are in, while the body is still arriving, so a
// transport that cancelled its context on the way out would truncate a perfectly
// good response into a spurious error — and only for responses big or slow
// enough not to be already buffered, which is to say only in production, where a
// 100-repo page is far larger than a test's. The handler below reproduces that
// by flushing its headers and then pausing before the body.
func TestAResponseBodyOutlivesTheRoundTripThatFetchedIt(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		require.True(t, ok, "the test server must support flushing for this to prove anything")
		flusher.Flush() // headers are out, so RoundTrip returns about here
		time.Sleep(50 * time.Millisecond)
		fmt.Fprint(w, `{"login":"octobot"}`)
	})
	c := newTestClient(t, mux)

	login, err := c.Verify(context.Background())

	require.NoError(t, err, "the body must survive RoundTrip returning — its deadline runs until Close")
	assert.Equal(t, "octobot", login)
}

// New is the only constructor production uses and it takes no arguments beyond
// the token, so nothing else can pin which timeout it picks: a New that quietly
// stopped passing requestTimeout would restore the unbounded default with every
// other test still green. Reading the value back off the built client also pins
// that it survives WithAuthToken's transport wrapping.
func TestNewAppliesTheRequestTimeout(t *testing.T) {
	c, err := New("t0ken")
	require.NoError(t, err)

	assert.Equal(t, requestTimeout, c.gh.Client().Timeout)
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
	assert.Equal(t, models.OwnerOrganization, ownerType)
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
	assert.Equal(t, models.OwnerUser, ownerType)
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
	assert.Equal(t, models.OwnerUser, ownerType)
	require.Len(t, repos, 1)
	assert.Equal(t, "me/private-thing", repos[0].FullName())
}

// TestListReposAuthenticatedOwnerPathIsCaseInsensitive locks the fix for a
// silent-wrong-answer bug. GitHub resolves logins case-insensitively, but
// c.login holds the one canonical spelling, so an operator who passes --org with
// different case (copied off a profile page) used to miss the `owner == c.login`
// fast path and fall through to /users/{login}/repos — whose contract is PUBLIC
// repositories only. The result was a frozen lockfile silently missing every
// private repo, with no error and no warning: everything downstream then works
// perfectly on a set the operator never asked for.
//
// The public-only endpoint is registered here rather than left to 404 on purpose,
// so a regression yields the WRONG SET instead of an error — which is exactly how
// the bug presented.
func TestListReposAuthenticatedOwnerPathIsCaseInsensitive(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"login":"redscaresu"}`)
	})
	mux.HandleFunc("/user/repos", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "[%s,%s]", repoJSON("redscaresu", "public-thing", nil, false), repoJSON("redscaresu", "private-thing", nil, false))
	})
	mux.HandleFunc("/users/RedScareSU", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"login":"redscaresu","type":"User"}`)
	})
	mux.HandleFunc("/users/RedScareSU/repos", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "[%s]", repoJSON("redscaresu", "public-thing", nil, false))
	})
	c := newTestClient(t, mux)

	repos, ownerType, err := c.ListRepos(context.Background(), "RedScareSU")
	require.NoError(t, err)
	assert.Equal(t, models.OwnerUser, ownerType)
	require.Len(t, repos, 2, "a case-mismatched own login must still reach the authenticated-user endpoint, which includes private repos")
	assert.Equal(t, []string{"public-thing", "private-thing"}, []string{repos[0].Name, repos[1].Name})
}

// TestOwnerTypeCaseInsensitiveFastPath pins the cheaper half of the same fix: the
// authenticated identity is always a User, so a case-mismatched own login needs
// no API round-trip. This path was never wrong (Users.Get resolves case-
// insensitively and returns the right type), only wasteful.
func TestOwnerTypeCaseInsensitiveFastPath(t *testing.T) {
	var lookups atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"login":"redscaresu"}`)
	})
	mux.HandleFunc("/users/", func(w http.ResponseWriter, r *http.Request) {
		lookups.Add(1)
		fmt.Fprint(w, `{"login":"redscaresu","type":"User"}`)
	})
	c := newTestClient(t, mux)

	got, err := c.OwnerType(context.Background(), "RedScareSU")
	require.NoError(t, err)
	assert.Equal(t, models.OwnerUser, got)
	assert.Zero(t, lookups.Load(), "the authenticated identity is always a User — no owner lookup should be needed")
}

func TestBranchExists(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/svc/branches/dev", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"name":"dev"}`)
	})
	mux.HandleFunc("/repos/acme/svc/branches/absent", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Branch not found"}`, http.StatusNotFound)
	})
	mux.HandleFunc("/repos/acme/svc/branches/boom", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	})
	c := newTestClient(t, mux)

	t.Run("present branch is true", func(t *testing.T) {
		has, err := c.BranchExists(context.Background(), "acme", "svc", "dev")
		require.NoError(t, err)
		assert.True(t, has)
	})

	t.Run("404 is false without error", func(t *testing.T) {
		has, err := c.BranchExists(context.Background(), "acme", "svc", "absent")
		require.NoError(t, err)
		assert.False(t, has)
	})

	t.Run("other errors propagate", func(t *testing.T) {
		_, err := c.BranchExists(context.Background(), "acme", "svc", "boom")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "acme/svc@boom")
	})
}

func TestGetRepo(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/svc", func(w http.ResponseWriter, r *http.Request) {
		// Owner object carries the type, which GetRepo reads back as the ownerType.
		fmt.Fprint(w, `{"name":"svc","owner":{"login":"acme","type":"Organization"},"clone_url":"https://github.com/acme/svc.git","default_branch":"dev","topics":["platform"],"archived":true}`)
	})
	mux.HandleFunc("/repos/acme/ghost", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	c := newTestClient(t, mux)

	t.Run("resolves repo and owner type, keeps archived", func(t *testing.T) {
		repo, ownerType, err := c.GetRepo(context.Background(), "acme", "svc")
		require.NoError(t, err)
		assert.Equal(t, models.OwnerOrganization, ownerType)
		assert.Equal(t, "acme/svc", repo.FullName())
		assert.Equal(t, "dev", repo.DefaultBranch)
		assert.Equal(t, "https://github.com/acme/svc.git", repo.CloneURL)
		assert.Equal(t, []string{"platform"}, repo.Topics)
		assert.True(t, repo.Archived, "an explicitly named archived repo is kept, not dropped")
	})

	t.Run("404 is a clear hard error", func(t *testing.T) {
		_, _, err := c.GetRepo(context.Background(), "acme", "ghost")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "acme/ghost")
		assert.Contains(t, err.Error(), "not found")
	})
}

func TestOwnerType(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"login":"me"}`)
	})
	mux.HandleFunc("/users/acme", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"login":"acme","type":"Organization"}`)
	})
	mux.HandleFunc("/users/bob", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"login":"bob","type":"User"}`)
	})
	mux.HandleFunc("/users/ghost", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	c := newTestClient(t, mux)

	t.Run("organization owner", func(t *testing.T) {
		ot, err := c.OwnerType(context.Background(), "acme")
		require.NoError(t, err)
		assert.Equal(t, models.OwnerOrganization, ot)
	})

	t.Run("user owner", func(t *testing.T) {
		ot, err := c.OwnerType(context.Background(), "bob")
		require.NoError(t, err)
		assert.Equal(t, models.OwnerUser, ot)
	})

	t.Run("the authenticated identity is a user without a lookup", func(t *testing.T) {
		ot, err := c.OwnerType(context.Background(), "me")
		require.NoError(t, err)
		assert.Equal(t, models.OwnerUser, ot)
	})

	t.Run("a missing owner errors", func(t *testing.T) {
		_, err := c.OwnerType(context.Background(), "ghost")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ghost")
	})
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
