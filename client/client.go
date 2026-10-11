// Package client wraps the read-only GitHub API surface goldfinger needs:
// resolving an owner's repositories for a selection. It is the only package
// that talks to the GitHub API, and it never mutates — writes are delegated to
// multi-gitter.
package client

import (
	"context"
	"fmt"
	"io"
	"net/http"
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

// Client is a thin, read-only GitHub API client for resolving a selection. Most
// of its work is a handful of calls (one auth check, one owner lookup, one page
// per 100 repos), but `select --branch-presence` adds one per repo per branch,
// so a large org can run to hundreds. It makes no attempt at rate-limit backoff:
// a limit error surfaces to the caller rather than being retried.
type Client struct {
	gh    *github.Client
	login string // authenticated user login, resolved by Verify
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
	}, extra...)

	gh, err := github.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("build GitHub client: %w", err)
	}
	return &Client{gh: gh}, nil
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
	u, _, err := c.gh.Users.Get(ctx, "")
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
		repos, err := c.paginate(func(page int) ([]*github.Repository, *github.Response, error) {
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
		repos, err := c.paginate(func(page int) ([]*github.Repository, *github.Response, error) {
			return c.gh.Repositories.ListByOrg(ctx, owner, &github.RepositoryListByOrgOptions{
				ListOptions: github.ListOptions{Page: page, PerPage: perPage},
			})
		})
		return repos, models.OwnerOrganization, err
	}
	repos, err := c.paginate(func(page int) ([]*github.Repository, *github.Response, error) {
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
	u, _, err := c.gh.Users.Get(ctx, owner)
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
	r, resp, err := c.gh.Repositories.Get(ctx, owner, name)
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
	_, resp, err := c.gh.Repositories.GetBranch(ctx, owner, repo, branch, 0)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return false, nil
		}
		return false, fmt.Errorf("check branch %s/%s@%s: %w", owner, repo, branch, err)
	}
	return true, nil
}

// paginate walks every page of a repo listing, following NextPage, and maps
// the results into models.Repo.
func (c *Client) paginate(fetch func(page int) ([]*github.Repository, *github.Response, error)) ([]models.Repo, error) {
	var out []models.Repo
	page := 1
	for {
		repos, resp, err := fetch(page)
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
