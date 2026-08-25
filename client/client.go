// Package client wraps the read-only GitHub API surface goldfinger needs:
// resolving an owner's repositories for a selection. It is the only package
// that talks to the GitHub API, and it never mutates — writes are delegated to
// multi-gitter.
package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-github/v89/github"
	"github.com/redscaresu/goldfinger/models"
)

// perPage is the max page size the REST API allows, minimising round-trips.
const perPage = 100

// requestTimeout bounds a single API request end to end — dial, TLS handshake,
// request write, response headers and body.
//
// Without it there is no bound at all: go-github builds a bare &http.Client{},
// whose zero Timeout means "wait forever", and the root context threaded from
// main carries no deadline of its own — it is cancelled by SIGINT/SIGTERM and
// nothing else. So a connection that stalls mid-request (a silently dropped TCP
// flow, a captive portal, a VPN flap) hung the run until an operator noticed and
// pressed Ctrl-C. This is deliberately fixed before any retry work: retrying on
// top of an unbounded request multiplies the hang rather than curing it.
//
// A whole-request wall clock is the right shape here only because nothing this
// client fetches streams — every response is one small JSON document, the
// largest being a 100-repo page — so there is no legitimate slow-but-progressing
// request for it to strangle. It is per request, not per run, so each retry
// added later starts a fresh budget.
//
// The asymmetry picks the value. Too generous costs extra waiting on a request
// that is almost certainly already dead; too tight costs a failed run that would
// have succeeded, paid on the success path and paid worst by the slowest links.
// 30s does not guarantee nothing legitimate is cut off — a response genuinely
// still arriving at 31s would be — it is a bet that for one small JSON document,
// thirty seconds of elapsed time means a stall rather than progress. It is
// roughly fifty times the latency any of these calls should show. The busiest
// path, `select --branch-presence` (one call per repo per branch), returns on
// the first error rather than continuing, so even a total outage costs one
// timeout rather than one per repo.
//
// It is applied twice, at two different layers, because neither layer catches
// everything the other does — see the transport wiring in newClient.
const requestTimeout = 30 * time.Second

// headerRetryAfter is the header GitHub uses to name how long to wait before
// the next request. go-github reads the same header but does not export the
// name.
const headerRetryAfter = "Retry-After"

// The retry budget for a failed API call.
//
// GitHub's rate limits are why this exists. A fleet-scale
// `select --branch-presence` issues one request per repo, and the *secondary*
// limit — a short burst cap, separate from the hourly 5,000 — is reachable long
// before the primary one. Both arrive as a response saying when to come back, so
// failing an entire selection on one is a worse answer than pausing for it.
//
// The retry sits inside each client method but above the go-github call, and
// that placement is forced. Below it — a retrying RoundTripper, which is where
// the request deadline already lives — sits underneath BranchExists's
// translation of 404 into "branch absent", and could not tell that from a
// transient 404. Retrying there would turn the single commonest response on the
// --branch-presence path into four requests and three sleeps, making the
// rate-limit problem worse in the exact loop this exists to protect. retryWait
// refuses a 404 outright as well, so the guarantee does not rest on placement
// alone.
const (
	// retryAttempts caps how many times one call is issued, the first try
	// included, so a limit that outlasts the budget ends the run rather than
	// looping.
	retryAttempts = 4

	// retryBaseWait is the first backoff, doubling per attempt (1s, 2s, 4s). It
	// applies only where GitHub named no delay of its own — a 5xx, or a limit
	// response whose reset is missing — since a delay GitHub does name is
	// honoured exactly, and a secondary limit that named none takes the floor
	// below instead. No jitter: one process calls this client serially, so there
	// is no fleet of callers to spread out.
	retryBaseWait = time.Second

	// maxRetryWait is the longest single pause goldfinger will hold a CLI open
	// for. It is chosen against the two real values GitHub sends: a secondary
	// limit typically asks for 60s, which is worth waiting out mid-run, while a
	// primary limit can be most of an hour away, which is not — that is a "come
	// back later", and withRetry says so rather than sleeping on it.
	maxRetryWait = 60 * time.Second

	// secondaryMinWait is the floor under a secondary limit that named no delay
	// of its own. GitHub's guidance for that exact case is explicit — "Otherwise,
	// wait for at least one minute before retrying" — and the backoff above would
	// come back in a second, which against a burst cap is the behaviour that
	// provoked it. It is equal to maxRetryWait rather than under it because a
	// minute is both the floor GitHub asks for and the longest goldfinger will
	// hold a run open: any less would disobey the guidance, any more would be a
	// wait it refuses to serve.
	secondaryMinWait = time.Minute

	// resetBuffer is slack added to a rate-limit reset. reset is a whole-second
	// timestamp from GitHub's clock, so retrying at exactly reset can land
	// inside the window that just ended and burn an attempt for nothing.
	// go-github's own reset sleep uses the same one-second buffer.
	resetBuffer = time.Second
)

// retryPolicy is the budget above, held as a value rather than read from the
// constants directly so a test can shrink the waits — no clock abstraction, and
// no sleeping through production-length pauses to prove a retry happened.
type retryPolicy struct {
	attempts     int
	base         time.Duration
	maxWait      time.Duration
	minSecondary time.Duration
}

func defaultRetryPolicy() retryPolicy {
	return retryPolicy{
		attempts:     retryAttempts,
		base:         retryBaseWait,
		maxWait:      maxRetryWait,
		minSecondary: secondaryMinWait,
	}
}

// Client is a thin, read-only GitHub API client for resolving a selection. Most
// of its work is a handful of calls (one auth check, one owner lookup, one page
// per 100 repos), but `select --branch-presence` adds one per repo per branch,
// so a large org can run to hundreds. A call GitHub answers with a rate limit is
// waited out and retried within a bounded budget, unless the delay GitHub names
// is longer than that budget will serve, in which case the run ends saying when
// to rerun. Every other failure — a 404 included — surfaces to the caller on the
// first answer.
type Client struct {
	gh    *github.Client
	login string // authenticated user login, resolved by Verify
	retry retryPolicy
}

// New builds a Client authenticated with the given PAT.
func New(token string) (*Client, error) {
	return newClient(token, requestTimeout)
}

// newClient holds all of the wiring, so New is only a choice of timeout and
// tests exercise the real construction path rather than a parallel one that
// could drift from it. extra is for a test's base URL override; production has
// none.
func newClient(token string, timeout time.Duration, extra ...github.ClientOptionsFunc) (*Client, error) {
	opts := append([]github.ClientOptionsFunc{
		github.WithAuthToken(token),
		github.WithTimeout(timeout),
		// The second half of the bound, and the half that covers this package's
		// highest-volume call. github.WithTimeout sets http.Client.Timeout,
		// which only reaches requests dispatched through http.Client.Do — and
		// some go-github methods are not. GetBranch (repos.go) goes via
		// roundTripWithOptionalFollowRedirect, which calls
		// c.client.Transport.RoundTrip directly (github.go), skipping the
		// http.Client layer and every deadline living on it. BranchExists is
		// that method, and `select --branch-presence` calls it once per repo per
		// branch — so the one path issuing hundreds of requests was the one path
		// a client-level timeout left unbounded.
		//
		// A round trip is the narrowest point every request passes through,
		// whichever layer above dispatched it, so bounding there closes that by
		// construction rather than by inspection.
		//
		// The two are complementary rather than redundant, and they divide by
		// path. On a bypassing path this transport is the only bound there is.
		// On an http.Client.Do path both apply and they differ over redirects: a
		// per-round-trip deadline gives every hop its own, whereas
		// http.Client.Timeout is a single budget across the whole chain — so the
		// pair gives Do calls an end-to-end bound the transport alone would not.
		// A redirect chain on a bypassing path would still be bounded only per
		// hop, but reaching one needs a caller passing a nonzero maxRedirects,
		// and BranchExists, the only bypassing call here, passes 0.
		github.WithTransport(&deadlineTransport{base: http.DefaultTransport, timeout: timeout}),
		// Leave the rate-limit policy to withRetry, which is the only one that
		// can see the whole picture, rather than running a second one underneath
		// it that cannot.
		//
		// go-github does not merely report a limit, it remembers one: on meeting a
		// secondary limit it records "issue no request before now+RetryAfter" and
		// answers every later call from that record alone, without a request
		// (bareDo and checkSecondaryRateLimitBeforeDo, github.go). It fills
		// RetryAfter from x-ratelimit-reset whenever GitHub named no Retry-After,
		// which is both the commonest secondary limit there is and precisely the
		// substitute retryWait declines to believe — so the record outlives the
		// wait served above it by as much as the rest of the hour, and the retries
		// the limit is owed get spent on cached refusals with nothing leaving the
		// process.
		//
		// Turning it off rather than shortening it is what keeps the reported
		// error honest. The one knob that shortens the record
		// (WithMaxSecondaryRateLimitRetryAfterDuration) does it by REWRITING
		// RetryAfter on the error it hands back, so a limit GitHub said to wait an
		// hour for would print, and answer errors.As with, whatever the cap was.
		// This also makes the two dispatch paths agree: BranchExists goes straight
		// at the transport and was never subject to any of it.
		github.WithDisableRateLimitCheck(),
	}, extra...)

	gh, err := github.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("build GitHub client: %w", err)
	}
	return &Client{gh: gh, retry: defaultRetryPolicy()}, nil
}

// deadlineTransport gives every round trip its own deadline, derived from the
// request's context so cancelling the run still wins.
type deadlineTransport struct {
	base    http.RoundTripper
	timeout time.Duration
}

func (t *deadlineTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(req.Context(), t.timeout)
	// RoundTrip must not mutate the request it is given; WithContext shallow-copies.
	resp, err := t.base.RoundTrip(req.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, err
	}
	// The deadline has to outlive RoundTrip, because the body is streamed and
	// read afterwards — cancelling here would truncate a good response into a
	// spurious error. So cancel is handed to Close instead.
	//
	// That is best effort rather than a guarantee, and deliberately so. On a
	// non-2xx response dispatched through Do, go-github's CheckResponse reads
	// the error body and then swaps r.Body for a NopCloser over the bytes it
	// read, discarding this wrapper before anything closes it — so cancel never
	// runs and the context lives out its deadline. (The bypassing paths do not
	// go through CheckResponse and do close this wrapper.) The fallback is the
	// design: an uncancelled context still releases itself when the deadline
	// lapses, so the worst case is a leak bounded by timeout rather than a
	// permanent one. net/http reaches for the same shape to clean up after
	// RoundTrip returns — Client.Timeout hangs its cleanup off a response-body
	// wrapper too (cancelTimerBody, client.go) — and loses it to the same swap,
	// so a body wrapper being best effort is what the standard library settles
	// for as well.
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// cancelOnClose releases a request's context once its body is closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelOnClose) Close() error {
	err := b.ReadCloser.Close()
	b.cancel() // context.CancelFunc is idempotent, so a double Close is safe.
	return err
}

// withRetry issues one go-github call, re-issuing it while the failure is one
// GitHub asked us to wait out. call must be idempotent; every call in this
// package is a GET.
//
// It returns the last attempt's value, response and error, so a caller's own
// status handling — GetRepo's and BranchExists's 404 branches — reads exactly
// the response it would have read without any retry.
func withRetry[T any](ctx context.Context, p retryPolicy, call func() (T, *github.Response, error)) (T, *github.Response, error) {
	for attempt := 0; ; attempt++ {
		v, resp, err := call()
		if err == nil {
			return v, resp, nil
		}
		wait, retryable := retryWait(resp, err, attempt, p)
		if !retryable {
			return v, resp, err
		}
		// GitHub named a wait longer than goldfinger will hold a run open for.
		// Retrying sooner than it asked is the one thing not to do: against the
		// primary limit it is certain to fail again, and against the secondary
		// limit it is the behaviour the limit exists to stop. So the honest
		// answer is to stop now and say when the run can resume, rather than
		// sleep the cap and fail anyway.
		//
		// Checked before the budget below, not after, because the two disagree
		// on the last attempt: there is no attempt left to spend either way, but
		// only this branch knows when the run can resume, and on GetBranch's
		// untyped path the error it would otherwise carry is "unexpected status
		// code: 403 Forbidden" — true, and no use to anyone.
		if wait > p.maxWait {
			return v, resp, fmt.Errorf("GitHub asked for %s before the next request, longer than goldfinger will wait (%s) — rerun after that: %w",
				wait.Round(time.Second), p.maxWait, err)
		}
		if attempt+1 >= p.attempts {
			// Out of budget with the limit still in force. The underlying error
			// is returned as it stands rather than wrapped in a count: on the
			// typed paths it already says what the limit was and when it lifts,
			// and the wait that would have been served is under the cap anyway,
			// so there is no "come back later" to add that the caller does not
			// already have.
			return v, resp, err
		}
		if werr := sleepFor(ctx, wait); werr != nil {
			return v, resp, fmt.Errorf("interrupted while waiting %s to retry after %v: %w",
				wait.Round(time.Second), err, werr)
		}
	}
}

// IsRateLimited reports whether GitHub refused the call over a rate limit — as
// opposed to rejecting the token — after the retry budget above declined to wait
// it out. It exists so a caller can tell "come back later" from "this will never
// work", without importing go-github to ask; the errors it recognises are wrapped
// all the way out, so it holds through the messages withRetry and each method add.
//
// It reads the typed errors only, which is the whole answer for a call dispatched
// through go-github's Do — but not for BranchExists, whose bypass leaves a limit
// untyped (see retryWait). No caller needs that today: doctor is the only one
// asking, and it asks about Verify.
func IsRateLimited(err error) bool {
	var limited *github.RateLimitError
	var abuse *github.AbuseRateLimitError
	return errors.As(err, &limited) || errors.As(err, &abuse)
}

// retryWait classifies a failed attempt: how long to wait before the next one,
// and whether there should be one at all.
//
// It reads two error shapes because this package produces both. Calls dispatched
// through go-github's Do come back as CheckResponse's typed errors. GetBranch
// does not — it round-trips the transport directly and returns a bare
// "unexpected status code" error (repos.go) — so on `select --branch-presence`,
// the highest-volume path in the package, a rate limit arrives untyped. Its
// *github.Response is populated either way, so the untyped path reads what the
// headers alone can settle: a 403 or 429 naming a Retry-After, or reporting no
// requests remaining, is a rate limit; a plain 403 is a permissions answer.
//
// That header rule is close to CheckResponse's but not identical to it, and the
// gap is worth naming. CheckResponse recognises a *secondary* limit from the
// response body's documentation_url (github.go), not from its headers. GetBranch
// closes the body before returning, so on that path the signal is gone, and a
// secondary limit that named no Retry-After is indistinguishable from a
// permissions denial: same status, same headers, and the one thing that told them
// apart has been read and discarded. Such a limit surfaces to the caller rather
// than being waited out. Retrying on suspicion instead would put a minute's pause
// on every SSO-protected org and every token missing a scope — far commoner
// answers than a secondary limit that named no delay, and ones no amount of
// waiting improves.
func retryWait(resp *github.Response, err error, attempt int, p retryPolicy) (time.Duration, bool) {
	// The secondary limit: a burst cap, and the one a per-repo loop meets first.
	var abuse *github.AbuseRateLimitError
	if errors.As(err, &abuse) {
		// Where GitHub named the wait, honour it exactly rather than guessing.
		//
		// Read from the header rather than from abuse.RetryAfter, which cannot be
		// trusted to mean what it says. go-github fills that field from
		// Retry-After if present and otherwise from x-ratelimit-reset
		// (parseSecondaryRate, github.go) — but that reset belongs to the PRIMARY
		// window, and GitHub's rule admits it only when the primary bucket is
		// spent, which here it provably is not: CheckResponse tests remaining=="0"
		// before it tests for a secondary limit, so reaching this branch means
		// remaining was something else. Taking the substitute at face value reads
		// a burst cap that wants a minute as a wait of however long the hour has
		// left to run — up to fifty-nine of them — and ends the run over a pause.
		if after, ok := retryAfterHeader(resp); ok && after > 0 {
			return after, true
		}
		// It named nothing usable: no Retry-After, or one that would not parse
		// (go-github discards that parse error, so an unreadable header arrives
		// as a substitute rather than as nothing). GitHub's guidance covers this
		// case by itself — "Otherwise, wait for at least one minute before
		// retrying" — and it outranks the backoff, which would come back in a
		// second, against the burst cap that just fired.
		//
		// Declining the substitute is only half of what that case needs, and the
		// other half is not here: go-github seeds its own "make no request before
		// now+RetryAfter" record from that very value, so a wait this code refuses
		// to serve is still one go-github refuses to make requests through, and
		// the retry would be answered from the record with nothing leaving the
		// process. Its rate-limit checking is switched off at construction for
		// that reason — see newClient — which is what leaves the wait served here
		// the only one in play.
		return max(backoff(attempt, p), p.minSecondary), true
	}
	// The primary limit: wait for the hourly window to roll over. Its reset is
	// read from the error rather than the headers because this shape always
	// carries one — CheckResponse builds it from the same response — whereas the
	// untyped path below has only what the headers say.
	var limited *github.RateLimitError
	if errors.As(err, &limited) {
		return limitWait(resp, limited.Rate.Reset.Time, attempt, p), true
	}
	if resp == nil || resp.Response == nil {
		// No response at all: a dial, TLS or request-deadline failure.
		// Deliberately not retried. Each attempt is bounded at requestTimeout,
		// so three more of them could add ninety seconds to a run that is
		// already failing, and none of these is a failure GitHub asked us to
		// wait out — which is what this budget is for.
		return 0, false
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		// Never retried, and said outright rather than left to fall through the
		// end of the switch, because this is the one that would hurt.
		// BranchExists turns a 404 into "branch absent" after this returns, so a
		// retried 404 would multiply the commonest response on the
		// --branch-presence path by the whole budget and sleep between each.
		return 0, false
	case resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusTooManyRequests:
		if resp.Header.Get(github.HeaderRateRemaining) == "0" {
			return limitWait(resp, resp.Rate.Reset.Time, attempt, p), true
		}
		if wait, ok := retryAfterHeader(resp); ok {
			return wait, true
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			// A 429 is a rate limit whatever else it carries — the status code
			// says so — and having named no delay it takes GitHub's floor for
			// exactly that case rather than the backoff.
			return max(backoff(attempt, p), p.minSecondary), true
		}
		// A 403 carrying no rate-limit signal is read as a permissions answer — a
		// token without the scope, an org behind SSO, a blocked repo. Retrying
		// spends quota to be told the same thing three more times. This is the
		// one shape where a secondary limit can hide, for the reason given above.
		return 0, false
	case resp.StatusCode >= 500:
		return backoff(attempt, p), true
	}
	return 0, false
}

// limitWait is the wait for a response that reported the quota spent, where
// GitHub may have named a Retry-After as well. It takes the longer of the two,
// because they are not alternatives to choose between: an empty bucket is a hard
// floor — every request before its reset gets the same answer — while
// Retry-After is a request not to come back sooner. Obeying only the shorter one
// breaks the other, and the failure is asymmetric: retrying into a bucket that
// has half an hour left burns the whole budget to be refused three more times,
// whereas the longer wait is either right or, if it exceeds the cap, ends the run
// with an honest "come back in half an hour". Deciding by precedence rather than
// by max is what makes this a trap — GitHub's guidance reads Retry-After first
// and go-github's CheckResponse reads the spent bucket first, so either order
// alone looks defensible and each ignores a signal that was present.
func limitWait(resp *github.Response, reset time.Time, attempt int, p retryPolicy) time.Duration {
	wait := untilReset(reset, attempt, p)
	if after, ok := retryAfterHeader(resp); ok && after > wait {
		return after
	}
	return wait
}

// retryAfterHeader reads GitHub's Retry-After, which it sends as a whole number
// of seconds. The HTTP-date form the RFC also permits is not used by this API,
// so an unparseable value is treated as absent rather than guessed at.
//
// The nil check is insurance rather than a live path. Both callers hold a typed
// error, which only CheckResponse builds and only from a real response — but a
// request that never got one produces no *github.Response at all, so a helper
// taking that pointer should not rely on its callers to have ruled it out.
func retryAfterHeader(resp *github.Response) (time.Duration, bool) {
	if resp == nil || resp.Response == nil {
		return 0, false
	}
	v := resp.Header.Get(headerRetryAfter)
	if v == "" {
		return 0, false
	}
	secs, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || secs < 0 {
		return 0, false
	}
	return time.Duration(secs) * time.Second, true
}

// untilReset is how long to wait for a rate-limit window to roll over. The two
// ways a reset can be unusable want opposite answers, so they are separated.
func untilReset(reset time.Time, attempt int, p retryPolicy) time.Duration {
	if reset.IsZero() {
		// Missing, or unparseable — go-github leaves the field zero either way.
		// The response said the bucket is spent but not when it refills, and a
		// second is no answer to that: every request until the real reset gets
		// the same refusal, so the backoff would spend the budget hammering a
		// bucket known to be empty. Fall back to the floor, which is the same
		// answer GitHub gives for any limit that named no delay.
		return max(backoff(attempt, p), p.minSecondary)
	}
	if wait := time.Until(reset) + resetBuffer; wait > 0 {
		return wait
	}
	// Already lapsed: the window rolled over between GitHub writing the response
	// and this reading it, so there is nothing left to wait for and the backoff
	// is the whole delay. Not the floor — that would sit out a minute for a
	// bucket that has already refilled.
	return backoff(attempt, p)
}

// backoff is the exponential wait for a failure GitHub named no delay for. It is
// clamped to the cap so it can never trip withRetry's give-up branch: that
// branch is for a wait GitHub dictated, not one goldfinger chose for itself.
func backoff(attempt int, p retryPolicy) time.Duration {
	wait := p.base << attempt
	if wait <= 0 || wait > p.maxWait { // wait <= 0 catches the shift overflowing
		return p.maxWait
	}
	return wait
}

// sleepFor waits d, or returns as soon as ctx is done. Cancellation has to win
// here as much as it does mid-request: without it a Ctrl-C during a
// rate-limit pause would sit unanswered for up to a minute at a time, which
// would give back exactly what the run's root context was added to provide.
func sleepFor(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err() // nil unless the run was already cancelled
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Verify confirms the token works and returns the authenticated user's login.
// It fails fast so a bad token surfaces before any repo work begins.
func (c *Client) Verify(ctx context.Context) (string, error) {
	if err := c.ensureLogin(ctx); err != nil {
		return "", err
	}
	return c.login, nil
}

func (c *Client) ensureLogin(ctx context.Context) error {
	if c.login != "" {
		return nil
	}
	u, _, err := withRetry(ctx, c.retry, func() (*github.User, *github.Response, error) {
		return c.gh.Users.Get(ctx, "")
	})
	if err != nil {
		return fmt.Errorf("authenticate with %s: %w", tokenName, err)
	}
	c.login = u.GetLogin()
	return nil
}

// ListRepos returns every repository owned by owner along with the owner's type
// ("User" or "Organization"), dispatching to the correct endpoint based on
// whether owner is the authenticated user, another user, or an organization.
func (c *Client) ListRepos(ctx context.Context, owner string) ([]models.Repo, string, error) {
	if err := c.ensureLogin(ctx); err != nil {
		return nil, "", err
	}
	// The authenticated user's own repos: use /user/repos so private repos
	// are included. The authenticated identity is always a user.
	//
	// EqualFold, because GitHub treats logins case-insensitively while c.login
	// holds the one canonical spelling. An operator who types --org RedScareSU
	// (copied off a profile page) would otherwise miss this branch and fall
	// through to ListByUser, whose contract is PUBLIC repositories only — a
	// selection silently missing every private repo, with no error and no
	// warning. The whole value of the frozen lockfile is that the set is
	// reviewable, and a repo that was never listed cannot be reviewed. The
	// lockfile validator already folds case on the same comparison
	// (selection/selection.go).
	if strings.EqualFold(owner, c.login) {
		repos, err := c.paginate(ctx, func(page int) ([]*github.Repository, *github.Response, error) {
			return c.gh.Repositories.ListByAuthenticatedUser(ctx, &github.RepositoryListByAuthenticatedUserOptions{
				Affiliation: "owner",
				ListOptions: github.ListOptions{Page: page, PerPage: perPage},
			})
		})
		return repos, models.OwnerUser, err
	}

	ownerType, err := c.OwnerType(ctx, owner)
	if err != nil {
		return nil, "", err
	}
	if ownerType == models.OwnerOrganization {
		repos, err := c.paginate(ctx, func(page int) ([]*github.Repository, *github.Response, error) {
			return c.gh.Repositories.ListByOrg(ctx, owner, &github.RepositoryListByOrgOptions{
				ListOptions: github.ListOptions{Page: page, PerPage: perPage},
			})
		})
		return repos, models.OwnerOrganization, err
	}
	repos, err := c.paginate(ctx, func(page int) ([]*github.Repository, *github.Response, error) {
		return c.gh.Repositories.ListByUser(ctx, owner, &github.RepositoryListByUserOptions{
			ListOptions: github.ListOptions{Page: page, PerPage: perPage},
		})
	})
	return repos, models.OwnerUser, err
}

// OwnerType resolves whether owner is a "User" or an "Organization" via a
// read-only lookup, without listing any repositories. select uses it to stamp a
// valid ownerType on an explicit selection (`--repo`/`--repos-from`) that
// resolved to zero repos under --allow-empty — there is no repo to read the type
// from, so an empty explicit lockfile would otherwise carry an empty (schema-
// invalid) ownerType, unlike an empty filtered one. The authenticated identity
// is always a User. It mutates nothing.
func (c *Client) OwnerType(ctx context.Context, owner string) (string, error) {
	if err := c.ensureLogin(ctx); err != nil {
		return "", err
	}
	// EqualFold for the same reason as ListRepos, though this path is not a
	// correctness bug: a case-mismatched own login would fall through to
	// Users.Get, which resolves logins case-insensitively and returns the right
	// type anyway. Folding here restores the fast path and saves a request.
	if strings.EqualFold(owner, c.login) {
		return models.OwnerUser, nil
	}
	u, _, err := withRetry(ctx, c.retry, func() (*github.User, *github.Response, error) {
		return c.gh.Users.Get(ctx, owner)
	})
	if err != nil {
		return "", fmt.Errorf("look up owner %q: %w", owner, err)
	}
	if u.GetType() == models.OwnerOrganization {
		return models.OwnerOrganization, nil
	}
	return models.OwnerUser, nil
}

// GetRepo resolves a single repository by owner/name via a read-only GET — the
// per-repo lookup an EXPLICIT selection (`select --repo` / `--repos-from`) needs,
// where the operator names the set instead of resolving a filter. It returns the
// mapped repo and the owner's type ("User" | "Organization"), taken from the
// repo's own owner object so an explicit selection records the same ownerType a
// filtered one would (mirror passes it to ghorg as --clone-type). A 404 (missing,
// renamed, or not visible to this token) is a clear hard error: a repo the
// operator named explicitly must fail loudly, never be silently dropped. Archived
// repos resolve normally — dropping them is discovery.Select's job, and an
// explicit selection deliberately keeps them. It mutates nothing.
func (c *Client) GetRepo(ctx context.Context, owner, name string) (models.Repo, string, error) {
	r, resp, err := withRetry(ctx, c.retry, func() (*github.Repository, *github.Response, error) {
		return c.gh.Repositories.Get(ctx, owner, name)
	})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return models.Repo{}, "", fmt.Errorf("repository %s/%s not found (deleted, renamed, or not visible to this token) — check the name under --org", owner, name)
		}
		return models.Repo{}, "", fmt.Errorf("look up repo %s/%s: %w", owner, name, err)
	}
	return toRepo(r), r.GetOwner().GetType(), nil
}

// BranchExists reports whether branch exists on owner/repo, via a read-only
// GET of the branch. A 404 means the branch is absent (false, no error); any
// other API error propagates so callers never mistake a transient failure for
// "branch missing". It mutates nothing — like the rest of this client, writes
// are multi-gitter's job.
func (c *Client) BranchExists(ctx context.Context, owner, repo, branch string) (bool, error) {
	// The 404 branch below is the reason the retry wraps the call here rather
	// than under it: "branch absent" is decided after withRetry has returned, so
	// the retry must never have treated that 404 as a failure worth repeating.
	// retryWait is where that is enforced.
	_, resp, err := withRetry(ctx, c.retry, func() (*github.Branch, *github.Response, error) {
		return c.gh.Repositories.GetBranch(ctx, owner, repo, branch, 0)
	})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return false, nil
		}
		return false, fmt.Errorf("check branch %s/%s@%s: %w", owner, repo, branch, err)
	}
	return true, nil
}

// Quota is a token's remaining REST budget for GitHub's "core" resource — the
// bucket every call goldfinger makes is billed to. Reset is when the window
// rolls over and the full Limit comes back.
type Quota struct {
	Limit     int
	Remaining int
	Reset     time.Time
}

// RemainingQuota reports how much core REST budget the token has left, so doctor
// can say whether a fleet-scale run has room before it starts one. It reads
// GitHub's dedicated rate-limit endpoint, which does not bill against the limit
// it reports — so asking costs nothing of what it measures, and the answer is
// not made worse by the asking. (GitHub does count it against the *secondary*
// limit, so it is cheap rather than free: one call inside a preflight, not
// something to poll.) It mutates nothing.
func (c *Client) RemainingQuota(ctx context.Context) (Quota, error) {
	limits, _, err := withRetry(ctx, c.retry, func() (*github.RateLimits, *github.Response, error) {
		return c.gh.RateLimit.Get(ctx)
	})
	if err != nil {
		return Quota{}, fmt.Errorf("read API rate limit: %w", err)
	}
	if limits == nil || limits.Core == nil {
		// Core is the only bucket goldfinger spends, so a response without it
		// carries no answer to the question asked. Say so rather than report a
		// zeroed Quota, which would read as an exhausted budget.
		return Quota{}, errors.New("read API rate limit: response carried no core budget")
	}
	return Quota{
		Limit:     limits.Core.Limit,
		Remaining: limits.Core.Remaining,
		Reset:     limits.Core.Reset.Time,
	}, nil
}

// paginate walks every page of a repo listing, following NextPage, and maps
// the results into models.Repo.
func (c *Client) paginate(ctx context.Context, fetch func(page int) ([]*github.Repository, *github.Response, error)) ([]models.Repo, error) {
	var out []models.Repo
	page := 1
	for {
		// Per page, not per listing: a retry re-fetches the page that failed and
		// the walk resumes, rather than restarting from page one.
		repos, resp, err := withRetry(ctx, c.retry, func() ([]*github.Repository, *github.Response, error) {
			return fetch(page)
		})
		if err != nil {
			return nil, fmt.Errorf("list repos: %w", err)
		}
		for _, r := range repos {
			out = append(out, toRepo(r))
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		page = resp.NextPage
	}
	return out, nil
}

func toRepo(r *github.Repository) models.Repo {
	return models.Repo{
		Owner:         r.GetOwner().GetLogin(),
		Name:          r.GetName(),
		CloneURL:      r.GetCloneURL(),
		DefaultBranch: r.GetDefaultBranch(),
		Topics:        r.Topics,
		Archived:      r.GetArchived(),
	}
}

// tokenName is referenced in error messages; kept in sync with the env var
// the CLI reads.
const tokenName = "GOLD_FINGER_PAT"
