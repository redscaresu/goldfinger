// Package client wraps the read-only GitHub API surface goldfinger needs:
// resolving an owner's repositories for a selection. It is the only package
// that talks to the GitHub API, and it never mutates — writes are delegated to
// multi-gitter.
package client

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/go-github/v89/github"
	"github.com/redscaresu/goldfinger/models"
)

// perPage is the max page size the REST API allows, minimising round-trips.
const perPage = 100

// Client is a thin, read-only GitHub API client for resolving a selection. Its
// call volume is tiny (one auth check, one owner lookup, one page per 100
// repos), so it makes no attempt at rate-limit backoff — a limit error, if it
// ever occurred, surfaces to the caller rather than being retried.
type Client struct {
	gh    *github.Client
	login string // authenticated user login, resolved by Verify
}

// New builds a Client authenticated with the given PAT.
func New(token string) (*Client, error) {
	gh, err := github.NewClient(github.WithAuthToken(token))
	if err != nil {
		return nil, fmt.Errorf("build GitHub client: %w", err)
	}
	return &Client{gh: gh}, nil
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
	if owner == c.login {
		repos, err := c.paginate(func(page int) ([]*github.Repository, *github.Response, error) {
			return c.gh.Repositories.ListByAuthenticatedUser(ctx, &github.RepositoryListByAuthenticatedUserOptions{
				Affiliation: "owner",
				ListOptions: github.ListOptions{Page: page, PerPage: perPage},
			})
		})
		return repos, models.OwnerUser, err
	}

	u, _, err := c.gh.Users.Get(ctx, owner)
	if err != nil {
		return nil, "", fmt.Errorf("look up owner %q: %w", owner, err)
	}
	if u.GetType() == models.OwnerOrganization {
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
