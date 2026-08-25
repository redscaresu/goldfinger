package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
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
	c.retry = testRetryPolicy()
	return c
}

// testRetryPolicy keeps the production retry SHAPE — same attempt count, same
// doubling, same give-up rule — and shrinks only the durations, so tests exercise
// the real loop without spending seven seconds asleep every time a case answers
// 5xx. The production values are pinned separately, by
// TestNewUsesTheProductionRetryBudget.
//
// maxWait is deliberately tiny: it makes the give-up branch reachable from a
// realistic Retry-After (GitHub's smallest real one is 60s) without a test ever
// waiting that long.
func testRetryPolicy() retryPolicy {
	return retryPolicy{
		attempts:     retryAttempts,
		base:         time.Millisecond,
		maxWait:      100 * time.Millisecond,
		minSecondary: 10 * time.Millisecond,
	}
}

// pastReset is a rate-limit reset that has already lapsed, rendered as GitHub
// sends it. Cases use it to send a fully realistic limit response whose wait
// resolves to the (tiny) backoff instead of the real pause a future reset would
// earn — which, being most of an hour, would not be waited out at all.
func pastReset() string {
	return strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10)
}

// writeRateLimited sends the response GitHub sends when a token is out of quota:
// the status, the exhausted-remaining header, and a reset. Every method's row
// uses it, but they do not all receive it the same way — see
// TestEveryCallRetriesARateLimitedRequest.
func writeRateLimited(w http.ResponseWriter, status int) {
	w.Header().Set(github.HeaderRateRemaining, "0")
	w.Header().Set(github.HeaderRateReset, pastReset())
	http.Error(w, `{"message":"API rate limit exceeded"}`, status)
}

// limitError builds a rate-limit error as go-github raises one, response and
// all. Both of its Error() methods render from r.Response.Request, so a bare
// literal panics the moment anything formats it — which fmt swallows into the
// message, leaving a test that reads as passing on a fixture nothing like the
// error it stands in for.
func limitError(kind string, reset time.Time) error {
	r := &http.Response{
		StatusCode: http.StatusForbidden,
		Request:    &http.Request{Method: http.MethodGet, URL: &url.URL{Scheme: "https", Host: "api.github.com", Path: "/user"}},
	}
	if kind == "secondary" {
		return &github.AbuseRateLimitError{Response: r, Message: "You have exceeded a secondary rate limit"}
	}
	return &github.RateLimitError{
		Response: r,
		Message:  "API rate limit exceeded",
		Rate:     github.Rate{Limit: 5000, Remaining: 0, Reset: github.Timestamp{Time: reset}},
	}
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
		{name: "RemainingQuota", login: "octobot", stallsOn: "/rate_limit", call: func(ctx context.Context, c *Client) error {
			_, err := c.RemainingQuota(ctx)
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

// Per method again, and for the same reason the timeout table is: go-github
// hands these two calls back in different shapes and the difference is invisible
// from the call site. A rate limit met through Do arrives as a typed
// *github.RateLimitError; met through GetBranch it arrives as a bare "unexpected
// status code" error, because that method round-trips the transport itself and
// never reaches the code that types the error. BranchExists is that method and
// `select --branch-presence` is the path that issues hundreds of requests, so
// the call likeliest to be rate limited is the one whose limit is hardest to
// recognise. A classifier that read only the typed errors would pass every row
// here but the last.
func TestEveryCallRetriesARateLimitedRequest(t *testing.T) {
	cases := []struct {
		name    string
		login   string
		respond map[string]string // prerequisite lookups, answered normally
		target  string            // the endpoint that rate-limits once, then succeeds
		body    string            // what it returns on the retry
		call    func(context.Context, *Client) error
	}{
		{name: "Verify", target: "/user", body: `{"login":"octobot"}`, call: func(ctx context.Context, c *Client) error {
			_, err := c.Verify(ctx)
			return err
		}},
		{name: "ListRepos/ownRepos", login: "acme", target: "/user/repos", body: `[]`, call: func(ctx context.Context, c *Client) error {
			_, _, err := c.ListRepos(ctx, "acme")
			return err
		}},
		{
			name:    "ListRepos/org",
			login:   "octobot",
			respond: map[string]string{"/users/acme": `{"login":"acme","type":"Organization"}`},
			target:  "/orgs/acme/repos",
			body:    `[]`,
			call: func(ctx context.Context, c *Client) error {
				_, _, err := c.ListRepos(ctx, "acme")
				return err
			},
		},
		{
			name:    "ListRepos/user",
			login:   "octobot",
			respond: map[string]string{"/users/acme": `{"login":"acme","type":"User"}`},
			target:  "/users/acme/repos",
			body:    `[]`,
			call: func(ctx context.Context, c *Client) error {
				_, _, err := c.ListRepos(ctx, "acme")
				return err
			},
		},
		{name: "OwnerType", login: "octobot", target: "/users/acme", body: `{"login":"acme","type":"Organization"}`, call: func(ctx context.Context, c *Client) error {
			_, err := c.OwnerType(ctx, "acme")
			return err
		}},
		{name: "GetRepo", login: "octobot", target: "/repos/acme/one", body: `{"name":"one","owner":{"login":"acme","type":"Organization"}}`, call: func(ctx context.Context, c *Client) error {
			_, _, err := c.GetRepo(ctx, "acme", "one")
			return err
		}},
		{name: "BranchExists", login: "octobot", target: "/repos/acme/one/branches/dev", body: `{"name":"dev"}`, call: func(ctx context.Context, c *Client) error {
			has, err := c.BranchExists(ctx, "acme", "one", "dev")
			assert.True(t, has, "the retry must return the branch, not the limit response")
			return err
		}},
		// The quota probe is on the same footing as the rest: doctor asking how
		// much budget is left is the one call an operator makes precisely when
		// the budget is short, so it is the last one that should give up on a
		// limit rather than wait it out.
		{name: "RemainingQuota", login: "octobot", target: "/rate_limit", body: `{"resources":{"core":{"limit":5000,"remaining":17,"reset":1700000000}}}`, call: func(ctx context.Context, c *Client) error {
			q, err := c.RemainingQuota(ctx)
			assert.Equal(t, 17, q.Remaining, "the retry must return the quota, not the limit response")
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
			var hits atomic.Int64
			mux.HandleFunc(tc.target, func(w http.ResponseWriter, _ *http.Request) {
				if hits.Add(1) == 1 {
					writeRateLimited(w, http.StatusForbidden)
					return
				}
				fmt.Fprint(w, tc.body)
			})
			c := newTestClient(t, mux)
			c.login = tc.login

			err := tc.call(context.Background(), c)

			require.NoError(t, err, "a rate limit that has already reset must be waited out and retried, not surfaced")
			assert.Equal(t, int64(2), hits.Load(), "the limited request should have been issued exactly twice")
		})
	}
}

// The commonest secondary limit there is — one GitHub sent with no Retry-After —
// and the only case where waiting the floor is not enough on its own to make the
// next attempt happen.
//
// go-github does not merely report that limit, it remembers it: on seeing one it
// records "issue no request before now+RetryAfter" and answers every later call
// from that record alone, without a request. It fills RetryAfter from
// x-ratelimit-reset when GitHub named no Retry-After — the primary window's
// reset, up to an hour out — so the record outlives the minute retryWait waits by
// as much as the rest of the hour, and the retries it is owed would be spent on
// cached refusals with nothing leaving the process. Switching that second policy
// off is what closes it, and that is done at construction, out of sight of
// retryWait.
//
// So this has to be asserted end to end, against the real go-github client:
// retryWait's own table cannot see a decision taken in newClient, and would go on
// passing with every retry going nowhere. What proves it is the server's hit
// count — the second request either reached the wire or it did not.
func TestASecondaryLimitWithNoStatedWaitStillReachesGitHub(t *testing.T) {
	var hits atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			// GitHub's secondary-limit response, exactly: no Retry-After, a
			// bucket that is NOT spent (so it types as secondary, not primary),
			// and the documentation_url CheckResponse keys on. The reset is an
			// hour out, which is what go-github will mistake for a retry delay.
			w.Header().Set(github.HeaderRateRemaining, "42")
			w.Header().Set(github.HeaderRateReset, strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
			http.Error(w, `{"message":"You have exceeded a secondary rate limit","documentation_url":"https://docs.github.com/rest/using-the-rest-api/rate-limits-for-the-rest-api#secondary-rate-limits"}`, http.StatusForbidden)
			return
		}
		fmt.Fprint(w, `{"login":"octobot"}`)
	})
	c := newTestClient(t, mux)

	login, err := c.Verify(context.Background())

	require.NoError(t, err, "a secondary limit that named no wait must be waited out and retried")
	assert.Equal(t, "octobot", login)
	assert.Equal(t, int64(2), hits.Load(), "the retry must reach GitHub, not go-github's own record of the limit")
}

// The trap this whole item had to avoid. BranchExists reads 404 as "branch
// absent", and it does so ABOVE the retry — so a retry that treated 404 as
// transient would turn the single commonest answer on the --branch-presence path
// into four requests and three sleeps per repo, burning quota fastest in the one
// loop the retry exists to protect.
//
// The 404 here also carries an exhausted rate-limit header, which is not
// contrived: the request that empties the bucket still gets its normal status,
// so a genuine "branch absent" can arrive alongside remaining=0. A classifier
// that keyed on the rate headers before the status would retry it.
func TestAnAbsentBranchIsAnsweredOnceAndNeverRetried(t *testing.T) {
	var hits atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/svc/branches/gone", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set(github.HeaderRateRemaining, "0")
		w.Header().Set(github.HeaderRateReset, pastReset())
		http.Error(w, `{"message":"Branch not found"}`, http.StatusNotFound)
	})
	c := newTestClient(t, mux)

	has, err := c.BranchExists(context.Background(), "acme", "svc", "gone")

	require.NoError(t, err)
	assert.False(t, has)
	assert.Equal(t, int64(1), hits.Load(), "a semantic 404 must cost exactly one request")
}

// A 403 is GitHub's answer both to "you are out of quota" and to "you may not
// see this" — a token missing a scope, an org behind SSO, a blocked repo. Only
// the first is worth another request; retrying the second spends quota to be
// told the same thing three more times, and delays the error the operator needs
// to read. The rate-limit headers are what separate them, so this case sends a
// 403 without any.
func TestAForbiddenWithoutARateLimitSignalIsNotRetried(t *testing.T) {
	var hits atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/private", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, `{"message":"Resource protected by organization SAML enforcement"}`, http.StatusForbidden)
	})
	c := newTestClient(t, mux)

	_, _, err := c.GetRepo(context.Background(), "acme", "private")

	require.Error(t, err)
	assert.Equal(t, int64(1), hits.Load(), "a permissions 403 must not be retried")
}

// The review's first constraint on the retry loop: it has to abort on
// cancellation rather than sleep through it. A backoff can be a minute long, and
// a Ctrl-C that goes unanswered for a minute gives back exactly what threading a
// root context through the run was for.
//
// The cancel is driven directly rather than through a test server, because the
// realistic-looking version proves less: cancelling on a signal from the handler
// races the client's read of that same response, so it lands mid-REQUEST,
// go-github returns the context error itself, and the loop under test is never
// entered at all. That version passed with the wait rewritten as a bare
// time.Sleep — the one implementation this test exists to reject.
//
// Both moments are covered, and the second is the one that matters. Cancelling
// before the wait starts is caught by any check at the top of the wait, including
// a check-once-then-sleep that ignores everything after it; only cancelling
// mid-wait forces the timer-and-select form. The wait is ten seconds against a
// one-attempt-and-a-retry budget, so a sleeping implementation overshoots the
// elapsed assertion by two orders of magnitude and issues the second request,
// while the correct one returns at once and never issues it. The limit is
// delivered in GetBranch's untyped shape, so the abort is pinned on the path
// where a rate limit is hardest to recognise.
func TestARetryWaitAbortsWhenTheRunIsCancelled(t *testing.T) {
	limited := &github.Response{Response: &http.Response{
		StatusCode: http.StatusForbidden,
		Header:     http.Header{github.HeaderRateRemaining: []string{"0"}},
	}}

	for _, tc := range []struct {
		name          string
		cancelInCall  bool
		cancelAfter   time.Duration
		wantAtMost    time.Duration
		wantedBecause string
	}{
		{
			name:          "cancelled before the wait begins",
			cancelInCall:  true,
			wantAtMost:    time.Second,
			wantedBecause: "a run already cancelled must not start a wait at all",
		},
		{
			name:          "cancelled while the wait is running",
			cancelAfter:   20 * time.Millisecond,
			wantAtMost:    2 * time.Second,
			wantedBecause: "the wait must end at cancellation, not run to its full length",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if !tc.cancelInCall {
				timer := time.AfterFunc(tc.cancelAfter, cancel)
				defer timer.Stop()
			}

			var attempts atomic.Int64
			start := time.Now()
			_, _, err := withRetry(ctx, retryPolicy{attempts: 2, base: 10 * time.Second, maxWait: time.Minute},
				func() (*github.Branch, *github.Response, error) {
					attempts.Add(1)
					if tc.cancelInCall {
						cancel()
					}
					return nil, limited, errors.New("unexpected status code: 403 Forbidden")
				})

			require.Error(t, err)
			assert.ErrorIs(t, err, context.Canceled, "a cancelled run must surface as cancellation, not as the limit that provoked the wait")
			assert.Less(t, time.Since(start), tc.wantAtMost, tc.wantedBecause)
			assert.Equal(t, int64(1), attempts.Load(), "the retry must not be issued after cancellation")
		})
	}
}

// A primary rate limit can reset the better part of an hour away. Waiting that
// out inside a CLI is not a retry, it is a hang; and retrying sooner than GitHub
// asked is the one thing not to do, since against the primary limit it is
// certain to fail again and against the secondary limit it is the behaviour the
// limit exists to stop. So the answer is to stop at once and say when the run
// can resume.
func TestAWaitLongerThanTheCapFailsAtOnceAndSaysWhen(t *testing.T) {
	var hits atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set(headerRetryAfter, "3600")
		http.Error(w, `{"message":"You have exceeded a secondary rate limit"}`, http.StatusForbidden)
	})
	c := newTestClient(t, mux)

	start := time.Now()
	_, err := c.Verify(context.Background())

	require.Error(t, err)
	assert.Equal(t, int64(1), hits.Load(), "no retry should be attempted once the wait is past the cap")
	assert.Less(t, time.Since(start), 5*time.Second, "it must fail immediately rather than sleep the cap first")
	assert.Contains(t, err.Error(), "1h0m0s", "the operator needs to be told how long to wait before rerunning")
}

// The same answer is owed on the LAST attempt as on the first, and the two are
// decided by different branches: by then the budget is spent, so an
// out-of-attempts check placed ahead of the over-cap one would return whatever
// the call last produced — on GetBranch's untyped path, "unexpected status code:
// 403 Forbidden", which is true and no use to anyone. The operator's position is
// identical either way: nothing left to try, and an hour to wait. Only one of
// those two branches knows the hour.
func TestAnOverCapWaitOnTheLastAttemptStillSaysWhen(t *testing.T) {
	var attempts atomic.Int64
	shortThenLong := func() (*github.Branch, *github.Response, error) {
		// The first failure is waitable, so the budget is genuinely spent by the
		// time the long one arrives, rather than the first response deciding it.
		after := "1"
		if attempts.Add(1) > 1 {
			after = "3600"
		}
		return nil, &github.Response{Response: &http.Response{
			StatusCode: http.StatusForbidden,
			Header:     http.Header{headerRetryAfter: []string{after}},
		}}, errors.New("unexpected status code: 403 Forbidden")
	}

	_, _, err := withRetry(context.Background(),
		retryPolicy{attempts: 2, base: time.Millisecond, maxWait: 10 * time.Second, minSecondary: time.Millisecond},
		shortThenLong)

	require.Error(t, err)
	assert.Equal(t, int64(2), attempts.Load(), "the budget should be spent, not abandoned early")
	assert.Contains(t, err.Error(), "1h0m0s", "an exhausted budget must not swallow the wait GitHub named")
}

// The distinction doctor spends a failed preflight on: a limit means come back
// later, a rejected token means fix the token, and telling an operator the second
// when it was the first sends them to rotate a PAT that was never the problem.
//
// Wrapping is the part that could quietly break it. The error reaches a caller
// through the retry loop's message and then the method's, so a %v anywhere in
// that chain would flatten the type and leave every limit looking like a bad
// credential — which is why the wrapped rows are here and not just the bare ones.
func TestIsRateLimited(t *testing.T) {
	primary := limitError("primary", time.Now().Add(40*time.Minute))
	deep := fmt.Errorf("authenticate with GOLD_FINGER_PAT: %w",
		fmt.Errorf("GitHub asked for 40m0s ... — rerun after that: %w", primary))

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "a primary limit", err: primary, want: true},
		{name: "a secondary limit", err: limitError("secondary", time.Time{}), want: true},
		{name: "a limit wrapped by the retry loop and the method", err: deep, want: true},
		{name: "a rejected token", err: errors.New("401 Bad credentials"), want: false},
		{name: "no error at all", err: nil, want: false},
		// The residual named in retryWait, restated as a fact about this
		// function: a limit BranchExists met has no type to recognise, so it
		// reads as a permissions answer here too.
		{name: "an untyped limit from the bypassing path", err: errors.New("unexpected status code: 403 Forbidden"), want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, IsRateLimited(tc.err))
		})
	}
}

// retryWait is where the timing decisions live, and they are the half that
// cannot be asserted through a server without sleeping for the durations under
// test. Testing it directly buys the real values — an honoured Retry-After, a
// reset genuinely in the future — at no wall-clock cost.
func TestRetryWait(t *testing.T) {
	p := retryPolicy{attempts: 4, base: time.Second, maxWait: 60 * time.Second, minSecondary: secondaryMinWait}
	fiveMinutes := 5 * time.Minute
	future := time.Now().Add(10 * time.Minute)
	zero := time.Duration(0)
	fortyFiveMinutes := 45 * time.Minute

	// resp builds the response go-github attaches to an untyped failure, with
	// the rate fields it parses out of the same headers.
	resp := func(status int, header http.Header) *github.Response {
		r := &github.Response{Response: &http.Response{StatusCode: status, Header: header}}
		if v := header.Get(github.HeaderRateReset); v != "" {
			secs, err := strconv.ParseInt(v, 10, 64)
			require.NoError(t, err)
			r.Rate.Reset = github.Timestamp{Time: time.Unix(secs, 0)}
		}
		return r
	}
	headers := func(kv ...string) http.Header {
		h := http.Header{}
		for i := 0; i+1 < len(kv); i += 2 {
			h.Set(kv[i], kv[i+1])
		}
		return h
	}

	cases := []struct {
		name      string
		resp      *github.Response
		err       error
		attempt   int
		wantRetry bool
		// wantWait is checked exactly when nonzero; wantAtLeast when the value
		// is derived from the wall clock and can only be bounded.
		wantWait    time.Duration
		wantAtLeast time.Duration
	}{
		{
			name:      "a secondary limit waits exactly as long as GitHub asked",
			resp:      resp(http.StatusForbidden, headers(headerRetryAfter, "300")),
			err:       &github.AbuseRateLimitError{RetryAfter: &fiveMinutes},
			wantRetry: true,
			wantWait:  fiveMinutes,
		},
		{
			// go-github fills RetryAfter from x-ratelimit-reset when there is no
			// Retry-After — but that reset is the PRIMARY window's, and GitHub
			// admits it only when the primary bucket is spent, which reaching
			// this branch proves it is not (CheckResponse tests remaining=="0"
			// first). Trusting the substitute reads a burst cap that wants a
			// minute as a wait of however long the hour has left, and ends the
			// run over a pause. So the field is ignored and the header is read
			// directly.
			name: "a secondary limit does not inherit the primary window's reset",
			resp: resp(http.StatusForbidden, headers(
				github.HeaderRateRemaining, "42",
				github.HeaderRateReset, strconv.FormatInt(time.Now().Add(45*time.Minute).Unix(), 10))),
			err:       &github.AbuseRateLimitError{RetryAfter: &fortyFiveMinutes},
			wantRetry: true,
			wantWait:  time.Minute,
		},
		{
			// GitHub's guidance for a secondary limit that names no delay is a
			// minute, and it outranks the backoff — which would come back in
			// four seconds, against the burst cap that just fired.
			name:      "a secondary limit with no stated wait takes GitHub's one-minute floor",
			err:       &github.AbuseRateLimitError{},
			attempt:   2,
			wantRetry: true,
			wantWait:  time.Minute,
		},
		{
			// A Retry-After that will not parse is discarded by go-github with
			// its error ignored, so it reaches the field as a zero rather than as
			// nothing. Read at face value that means "retry now", and would fire
			// the budget back to back during the one limit that exists to stop
			// exactly that. Reading the header instead settles it: unparseable
			// and absent are the same answer.
			name:      "a secondary limit whose stated wait will not parse takes the floor instead",
			resp:      resp(http.StatusForbidden, headers(headerRetryAfter, "Wed, 21 Oct 2015 07:28:00 GMT")),
			err:       &github.AbuseRateLimitError{RetryAfter: &zero},
			wantRetry: true,
			wantWait:  time.Minute,
		},
		{
			name:        "a primary limit waits for its window to roll over",
			err:         &github.RateLimitError{Rate: github.Rate{Reset: github.Timestamp{Time: future}}},
			wantRetry:   true,
			wantAtLeast: 9 * time.Minute,
		},
		{
			// A window that rolled over between GitHub writing the response and
			// this reading it has nothing left to wait for, so this one takes the
			// backoff and not the floor — unlike the missing reset below, which
			// looks similar and wants the opposite answer.
			name:      "a primary limit whose reset already lapsed falls back to the backoff",
			err:       &github.RateLimitError{Rate: github.Rate{Reset: github.Timestamp{Time: time.Now().Add(-time.Hour)}}},
			wantRetry: true,
			wantWait:  time.Second,
		},
		{
			// The bucket is spent and the response never said when it refills.
			// Every request until the real reset gets the same refusal, so a
			// one-second backoff would spend the budget hammering a bucket known
			// to be empty.
			name:      "a spent bucket that never said when it refills waits the floor, not the backoff",
			resp:      resp(http.StatusForbidden, headers(github.HeaderRateRemaining, "0")),
			err:       errors.New("unexpected status code: 403 Forbidden"),
			wantRetry: true,
			wantWait:  time.Minute,
		},
		{
			// The GetBranch shape: no typed error, so the same signals have to
			// be read off the headers.
			name:        "an untyped 403 with the quota exhausted waits for the reset",
			resp:        resp(http.StatusForbidden, headers(github.HeaderRateRemaining, "0", github.HeaderRateReset, strconv.FormatInt(future.Unix(), 10))),
			err:         errors.New("unexpected status code: 403 Forbidden"),
			wantRetry:   true,
			wantAtLeast: 9 * time.Minute,
		},
		{
			name:      "an untyped 403 carrying Retry-After honours it",
			resp:      resp(http.StatusForbidden, headers(headerRetryAfter, "45")),
			err:       errors.New("unexpected status code: 403 Forbidden"),
			wantRetry: true,
			wantWait:  45 * time.Second,
		},
		{
			// Both signals at once, which is where deciding by precedence goes
			// wrong: GitHub's guidance reads Retry-After first, go-github's
			// CheckResponse reads the spent bucket first, and either order alone
			// ignores a signal that was present. Coming back in a minute to a
			// bucket with ten more minutes on it just spends the rest of the
			// budget being refused.
			name: "a spent bucket outranks a shorter Retry-After",
			resp: resp(http.StatusTooManyRequests, headers(
				headerRetryAfter, "60",
				github.HeaderRateRemaining, "0",
				github.HeaderRateReset, strconv.FormatInt(future.Unix(), 10))),
			err:         errors.New("unexpected status code: 429 Too Many Requests"),
			wantRetry:   true,
			wantAtLeast: 9 * time.Minute,
		},
		{
			// And the other way round, on the typed path, so the rule is the
			// longer of the two rather than a second precedence in disguise.
			name:      "a longer Retry-After outranks a nearer reset",
			resp:      resp(http.StatusForbidden, headers(headerRetryAfter, "300")),
			err:       &github.RateLimitError{Rate: github.Rate{Reset: github.Timestamp{Time: time.Now().Add(10 * time.Second)}}},
			wantRetry: true,
			wantWait:  5 * time.Minute,
		},
		{
			// The HTTP-date form the RFC permits but this API does not use. It
			// is treated as absent — the 429 falls through to the floor for a
			// limit that named no delay — rather than guessed at, which is the
			// failure mode where a misread header becomes a wrong wait.
			name:      "a Retry-After that will not parse is treated as absent, not guessed at",
			resp:      resp(http.StatusTooManyRequests, headers(headerRetryAfter, "Wed, 21 Oct 2015 07:28:00 GMT")),
			err:       errors.New("unexpected status code: 429 Too Many Requests"),
			wantRetry: true,
			wantWait:  time.Minute,
		},
		{
			// A 429 is a rate limit by status code whatever else it carries, so
			// unlike the bare 403 below it is retried rather than read as a
			// permissions answer.
			name:      "a bare 429 is retried on the same floor",
			resp:      resp(http.StatusTooManyRequests, headers()),
			err:       errors.New("unexpected status code: 429 Too Many Requests"),
			wantRetry: true,
			wantWait:  time.Minute,
		},
		{
			// The deliberate residual, pinned here so it is a decision rather
			// than an oversight. A secondary limit that named no Retry-After
			// arrives in this exact shape once GetBranch has discarded the body
			// CheckResponse would have recognised it by, so this row is not
			// purely a permissions answer — it is the two of them, indistinguish-
			// able. It is not retried because the permissions half is far the
			// commoner (every SSO-protected org, every token missing a scope) and
			// no amount of waiting improves it, so retrying on suspicion would
			// put a minute's pause on the usual answer to buy the rare one.
			name:      "a 403 with no rate-limit signal is read as a permissions answer",
			resp:      resp(http.StatusForbidden, headers()),
			err:       errors.New("unexpected status code: 403 Forbidden"),
			wantRetry: false,
		},
		{
			name:      "a 404 is never retried",
			resp:      resp(http.StatusNotFound, headers(github.HeaderRateRemaining, "0")),
			err:       errors.New("unexpected status code: 404 Not Found"),
			wantRetry: false,
		},
		{
			name:      "a server error is retried on the backoff",
			resp:      resp(http.StatusBadGateway, headers()),
			err:       errors.New("unexpected status code: 502 Bad Gateway"),
			attempt:   1,
			wantRetry: true,
			wantWait:  2 * time.Second,
		},
		{
			// A dial, TLS or request-deadline failure. Each attempt is bounded
			// at requestTimeout, so retrying could add ninety seconds to a run
			// that is already failing, and none of these is a wait GitHub asked
			// for — which is what this budget is for.
			name:      "a failure with no response at all is not retried",
			err:       errors.New("dial tcp: connection refused"),
			wantRetry: false,
		},
		{
			name:      "a client error that is not a limit is not retried",
			resp:      resp(http.StatusUnprocessableEntity, headers()),
			err:       errors.New("unexpected status code: 422"),
			wantRetry: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wait, retry := retryWait(tc.resp, tc.err, tc.attempt, p)

			assert.Equal(t, tc.wantRetry, retry)
			switch {
			case tc.wantWait != 0:
				assert.Equal(t, tc.wantWait, wait)
			case tc.wantAtLeast != 0:
				assert.Greater(t, wait, tc.wantAtLeast)
			}
		})
	}
}

// New is the only constructor production uses, so nothing else pins which retry
// budget it picks: a New that quietly stopped installing one would leave every
// call unretried with the rest of this file still green, because the tests set
// their own policy to keep the waits short.
func TestNewUsesTheProductionRetryBudget(t *testing.T) {
	c, err := New("t0ken")
	require.NoError(t, err)

	assert.Equal(t, defaultRetryPolicy(), c.retry)
	assert.Equal(t, retryAttempts, c.retry.attempts)
	assert.Equal(t, maxRetryWait, c.retry.maxWait)
	assert.Equal(t, time.Minute, c.retry.minSecondary, "GitHub's floor for a secondary limit that named no delay is one minute")
	assert.LessOrEqual(t, c.retry.minSecondary, c.retry.maxWait,
		"a floor above the cap would make every unnamed secondary limit end the run instead of waiting it out")
}

func TestRemainingQuota(t *testing.T) {
	reset := time.Now().Add(42 * time.Minute).Truncate(time.Second)

	t.Run("reports the core budget", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/rate_limit", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, `{"resources":{"core":{"limit":5000,"remaining":4211,"reset":%d},"search":{"limit":30,"remaining":30,"reset":%d}}}`,
				reset.Unix(), reset.Unix())
		})
		c := newTestClient(t, mux)

		q, err := c.RemainingQuota(context.Background())
		require.NoError(t, err)
		assert.Equal(t, Quota{Limit: 5000, Remaining: 4211, Reset: reset}, q,
			"core is the bucket every goldfinger call is billed to; search is not read")
	})

	// A zeroed Quota would read as an exhausted budget, which is the opposite of
	// "I could not tell" — and would have doctor warn about a quota nobody
	// measured.
	t.Run("a response without a core budget is an error, not an empty quota", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/rate_limit", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"resources":{"search":{"limit":30,"remaining":30,"reset":0}}}`)
		})
		c := newTestClient(t, mux)

		_, err := c.RemainingQuota(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "core")
	})
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
