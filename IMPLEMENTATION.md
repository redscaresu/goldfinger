# goldfinger — Implementation Plan (v0.1)

## Start here (for a fresh implementing agent)

You are implementing this plan from scratch with no prior conversation context.
Everything you need is in this repo:

1. Read `README.md` first — it's the *why* and the product design. This file is
   the *what and how*; where they differ on detail, this file wins.
2. Design questions are settled. The "Builder handoff notes" section pins the
   decisions; the "Deliberately out" section is a hard scope fence. Do not add
   features from README sections marked v0.2/later.
3. Work the **Build order** steps in sequence. One step = one commit (or a few
   small ones); make each step's acceptance criteria pass before starting the
   next. Don't scaffold later steps early.
4. Things only the human can provide — stop and ask rather than working around
   them:
   - a `GITHUB_TOKEN` PAT for live testing (never ask them to paste it into
     chat; have them export it in their shell)
   - the name of a test org with 2–3 disposable repos for step 4's live run
   - execution of any non-dry-run `run` against real repos
5. Live API smoke tests (step 2's `goldfinger repos`, step 4's dry-run) are
   fine to run yourself once a token is in the environment — they're
   read-only. Anything that pushes or opens a PR is the human's to run.
6. There is no CI yet. Your loop is `go build ./...`, `go vet ./...`,
   `go test ./...` — all three clean before every commit.

Scope for the first working version, per README:

- Auth: personal access token from `GITHUB_TOKEN`. No other auth modes.
- Targeting: one org via `--org` (required). Repo selection is **exactly one of**:
  - `--all-repos` — every non-archived repo in the org
  - `--topic <t>` — repeatable; repo matches if it has **any** of the given topics
- Commands: `repos` (discovery only) and `run` (clone → script → commit → push → PR).
  Campaign lifecycle (`status`/`merge`/`close`) is deferred to v0.2.

## CLI surface

```
goldfinger repos --org <org> (--all-repos | --topic <t> [--topic <t>...])

goldfinger run   --org <org> (--all-repos | --topic <t>...)
                 --branch <name>
                 --commit-message <msg>
                 --pr-title <title>
                 [--pr-body <body>]
                 [--label <l>...]       # applied to every PR (campaign tracking)
                 [--reviewer <user-or-org/team>...]
                 [--draft]              # open PRs as drafts
                 [--concurrency N]      # default 10 (clone/script pool; PR creation is throttled separately)
                 [--sparse <path>...]   # sparse checkout: only fetch these paths (fast on huge repos)
                 [--max-repos N]        # default 50; refuse to exceed without explicit flag
                 [--dry-run]            # clone + script + diff, no push/PR
                 [--output table|json]  # default table
                 -- <command> [args...] # the per-repo script, after the -- separator
```

Flag rules, enforced in `cmd/` before any API call:

- `--org` required; `GITHUB_TOKEN` must be non-empty (fail fast with a clear message).
- `--all-repos` and `--topic` are mutually exclusive, and one is required — there
  is no implicit default to the whole org.
- `run` additionally requires `--branch`, `--commit-message`, `--pr-title`, and a
  script after `--`.

## Package layout and responsibilities

Flat top-level packages per [simpleAPI](https://github.com/redscaresu/simpleAPI),
tests alongside code:

```
cmd/        main.go — cobra wiring, flag parsing/validation, output rendering
models/     shared domain types, no dependencies on other goldfinger packages
client/     GitHub API access (go-github): list org repos, create PR
discovery/  turns CLI targeting flags into a filtered []models.Repo
gitexec/    thin wrapper over the system git binary
campaign/   the run engine: worker pool, per-repo pipeline, result collection
```

### models

```go
type Repo struct {
    Owner         string
    Name          string
    CloneURL      string   // https
    DefaultBranch string
    Topics        []string
    Archived      bool
}

type Status string // StatusSuccess | StatusSkipped | StatusFailed

type RepoResult struct {
    Repo   Repo
    Status Status
    PRURL  string // set on success
    Err    error  // set on failure; script stderr captured in the message
}

type RunSpec struct { // everything campaign needs, built in cmd/
    Branch, CommitMessage, PRTitle, PRBody string
    Labels      []string
    Reviewers   []string // users or org/team slugs
    Draft       bool
    Script      []string
    Concurrency int
    DryRun      bool
}
```

### client

Wraps `google/go-github`. Constructed with the token; nothing else reads env vars.

```go
func New(token string) *Client
func (c *Client) ListOrgRepos(ctx context.Context, org string) ([]models.Repo, error)
func (c *Client) CreatePR(ctx context.Context, repo models.Repo, spec models.RunSpec) (url string, err error)
```

- `ListOrgRepos` pages through `GET /orgs/{org}/repos` (topics are included in
  the listing response — no per-repo topic calls).
- `CreatePR` treats "PR already exists for this branch" (422) as success: fetch
  and return the existing PR's URL, so re-runs are idempotent.
- After creation it applies labels and requests reviewers (two further API
  calls, each through the mutation bucket — so per-repo mutation cost is ~3–4
  calls, not 2; the pre-run API estimate accounts for this). Label/reviewer
  failures downgrade to a warning on the result, not a failed repo — the PR
  exists, which is what matters.
- `--draft` is a field on the create call itself, no extra cost.
- Backoff: on `403`/`429` with `Retry-After` (secondary rate limits), sleep and
  retry up to 3 times. This lives here so callers never see rate-limit errors
  they can't handle.

Tests: `httptest.Server` with canned JSON pages; assert pagination is followed,
422-on-existing-PR returns the existing URL, and backoff retries on 403.

### discovery

Pure filtering — takes the full repo list from `client`, applies targeting:

```go
type Filter struct {
    AllRepos bool
    Topics   []string // match if repo has ANY of these
}
func Select(repos []models.Repo, f Filter) []models.Repo
```

- Always excludes archived repos.
- Kept separate from `client` so filter logic is testable with plain structs, no
  HTTP involved.

Tests: table-driven over topic/archived combinations with `assert.Equal`.

### gitexec

Shells out to system `git` via `exec.CommandContext`; every function takes a
working directory and returns wrapped stderr on failure.

```go
func Clone(ctx context.Context, cloneURL, token, dir string) error // --depth 1, token embedded in URL for push auth
func NewBranch(ctx context.Context, dir, name string) error        // checkout -b
func IsDirty(ctx context.Context, dir string) (bool, error)        // status --porcelain
func Diff(ctx context.Context, dir string) (string, error)         // diff (for --dry-run)
func CommitAll(ctx context.Context, dir, message string) error     // add -A && commit
func Push(ctx context.Context, dir, branch string) error           // push -u origin, force-with-lease for idempotent re-runs
```

Token handling: the token goes into the clone URL's userinfo for push auth but
must never appear in error messages — redact it when wrapping command output.

Tests: integration-style against real `git` — create a local bare repo in
`t.TempDir()`, use it as the remote, and assert the full clone → branch →
commit → push cycle. No mocks; this package's whole job is driving real git.

### campaign

The engine. Depends on `client`, `gitexec`, `discovery` output.

```go
func Run(ctx context.Context, c *client.Client, repos []models.Repo, spec models.RunSpec) []models.RepoResult
```

Per-repo pipeline (one goroutine per repo, bounded by `errgroup.Group.SetLimit`):

1. Clone into a per-repo temp dir (always `defer os.RemoveAll`).
2. Create the campaign branch.
3. Run the script: CWD = working tree, env = parent env + `GOLDFINGER_REPO=owner/name`.
   - exit non-zero → `StatusFailed`, capture stderr, stop.
   - tree clean → `StatusSkipped`, stop.
4. `--dry-run` → collect the diff into the result, stop before any push.
5. Commit, push, create PR → `StatusSuccess` with PR URL.

Error containment: one repo failing never cancels the others — errors go into
`RepoResult.Err`, never up through errgroup. Ctrl-C (context cancellation) does
stop the fleet.

Tests: fake the GitHub client behind a small interface; use real `gitexec`
against local bare remotes; scripts are tiny shell one-liners
(`touch changed.txt` for dirty, `true` for clean, `exit 1` for failure). Assert
each path yields the right `Status`, and that skipped repos open no PR.

### cmd

- cobra root + `repos` + `run` subcommands; all flag validation up front.
- Wires: token → `client.New` → `ListOrgRepos` → `discovery.Select` →
  (`repos`: print) / (`run`: confirm count, then `campaign.Run`).
- `run` prints the selected repo count and refuses to proceed past `--max-repos`
  (default 50) without the flag raised — the guard against an accidental
  org-wide blast.
- Output: aligned table by default (`repo  status  pr-url/error`); `--output json`
  emits `[]RepoResult` for CI.

## Auth, identity, and signing

- **PAT scopes.** Classic PAT needs `repo` (covers private repos, contents, and
  PRs). Fine-grained PAT needs: Metadata (read), Contents (read/write), Pull
  requests (read/write) on the target org's repos. `cmd/` verifies the token
  works with a cheap `GET /user` before doing anything else, so a bad token
  fails in one second, not after 50 clones.
- **Push model.** Direct push to origin on every repo — the PAT is assumed to
  have write access org-wide. The fork workflow (fork → push to fork →
  cross-repo PR) is explicitly out of scope; a repo we can't push to is simply
  a **failed** result with a clear error.
- **Commit identity.** Commits use the operator's normal git config
  (`user.name`/`user.email`), inherited automatically since we shell out to
  real git. `run` fails fast at startup if they're unset.
- **Commit signing: supported for free, with one caveat.** Because commits are
  made by the real git CLI, `commit.gpgsign`/`gpg.format` from the operator's
  git config just work — signing a commit is milliseconds of local CPU, and at
  one commit per repo it has zero measurable effect on fleet speed. The caveat
  is interactivity, not speed: a GPG key that pops a passphrase prompt will
  hang a 300-repo run at full concurrency. If signing is required, use SSH
  signing (`gpg.format ssh`) with an agent-loaded key so it's non-interactive.
  We deliberately do **not** use the "GitHub API commits are auto-signed" trick:
  API-created commits are content mutations, so they'd queue behind the 1/sec
  throttle — that path is *slower* than local signing, not faster.

## Build order

Each step compiles, is tested, and is independently reviewable:

1. **Skeleton** — `go mod init github.com/redscaresu/goldfinger`, cobra root
   command, `models`, token/flag validation. Deliverable: `goldfinger --help`.
2. **client + discovery + `repos`** — list and filter org repos. Deliverable:
   `goldfinger repos --org X --topic platform` prints real repos. First point of
   end-to-end validation against the live API.
3. **gitexec** — full local clone/branch/commit/push cycle with tests against
   local bare repos.
4. **campaign + `run` (dry-run first)** — engine wired end-to-end with
   `--dry-run` as the default development mode; then enable push + PR.
   Deliverable: a real PR opened across a 2–3 repo test org.
5. **Hardening** — `--output json`, token redaction audit, secondary-rate-limit
   backoff verified, README updated with real usage.

Dependencies: cobra, `google/go-github`, `golang.org/x/sync/errgroup`,
testify. Nothing else.

## Builder handoff notes

This plan will be implemented by a coding agent. These notes pin decisions a
builder would otherwise have to guess, and set the guardrails.

**Pinned decisions — do not re-litigate:**

- Module path `github.com/redscaresu/goldfinger`. Latest stable Go.
- Dependencies: `github.com/spf13/cobra`, `github.com/google/go-github/v68`
  (or current major), `golang.org/x/sync/errgroup`, `golang.org/x/time/rate`
  (mutation bucket), `github.com/stretchr/testify`. **Nothing else** without
  asking.
- The GitHub client is consumed by `campaign` through a small interface defined
  in `campaign` (consumer side), satisfied by `*client.Client` — that's the
  mocking seam:

  ```go
  type GitHub interface {
      CreatePR(ctx context.Context, repo models.Repo, spec models.RunSpec) (string, error)
  }
  ```
- Pagination: use go-github's `Response.NextPage`/`LastPage` — do not hand-parse
  `Link` headers.
- "PR already exists" detection: on 422 from PR create, list PRs with
  `head=owner:branch`; if one exists, return its URL as success.
- Push: `git push --force-with-lease -u origin <branch>`.
- Sparse path: `git clone --depth 1 --filter=blob:none --sparse` then
  `git sparse-checkout set <paths...>`.
- Token redaction: the clone URL embeds the token
  (`https://x-access-token:<tok>@github.com/...`); before any error/log output
  leaves `gitexec`, `strings.ReplaceAll(out, token, "***")`. There must be a
  test proving a failed push's error does not contain the token.

**Per-step acceptance criteria** (each build-order step is done when):

1. `go build ./...` clean; `goldfinger --help` and both subcommands' `--help`
   render; missing `GITHUB_TOKEN` / missing `--org` / both-or-neither of
   `--all-repos`/`--topic` each produce a one-line actionable error. Unit tests
   for flag validation pass.
2. `goldfinger repos --org <org> --topic x` prints `owner/name` lines against
   the live API. Pagination proven by an `httptest` test serving 3 pages;
   topic/archived filtering covered by table tests.
3. `go test ./gitexec/...` passes using real git against `t.TempDir()` bare
   remotes, covering: clone, branch, dirty/clean detection, commit, push,
   force-with-lease re-push, and the token-redaction test.
4. `goldfinger run --dry-run` against local fixture remotes yields correct
   per-repo statuses (success-diff / skipped / failed with captured stderr) and
   cleans up all temp dirs. Then live: PRs (with label, reviewer, draft) opened
   on a 2–3 repo test org — **the human runs the live step and confirms**; the
   agent must never execute non-dry-run `run` against a real org unprompted.
5. `--output json` round-trips through `json.Unmarshal` in a test; every
   `RepoResult` includes phase timings; `go vet ./...` and `go test ./...`
   clean.

**Guardrails:**

- No config files, no env vars beyond `GITHUB_TOKEN`, no flags beyond the CLI
  surface above. If a capability seems missing, stop and ask — don't invent
  surface area.
- No interfaces or abstraction layers beyond the one `GitHub` interface above
  until a second implementation actually exists (YAGNI).
- Follow the repo structure exactly (flat packages, simpleAPI style); tests use
  testify per the global guidelines.
- Every error wrapped with repo context: `fmt.Errorf("repo %s: clone: %w", ...)`
  — a 300-repo run's failure report is unusable without it.
- If a step's acceptance criteria can't be met as written, stop and report —
  do not redefine the criteria.

## Performance plan

Part of the tool's reason to exist is being faster than the GraphQL-scripted
workflow. That workflow is slow for two compounding reasons: it's sequential
(one repo at a time through search → clone → mutate), and it routes everything
through the GraphQL API, whose point-based rate limit (~5,000 points/hr) meters
searches, reads, and mutations out of the same budget.

**The design rule that makes goldfinger fast: do the work over the git
protocol, not the API.** Clone, fetch, and push are unmetered — they don't
touch any API rate limit and parallelize as wide as the machine allows. The API
is spent only on the two things git cannot do — discovering repos and opening
the PR — so the metered budget per repo is ~2–4 calls instead of the GraphQL
workflow's search + read + mutation chain. Everything below follows from
knowing which work parallelizes freely and which is capped by GitHub.

**The bottleneck model.** A run has three cost centers:

| Phase | Bound by | Parallelizable? |
|---|---|---|
| Discovery (list org repos) | API pagination | yes — pages fetched concurrently |
| Clone + script | network + disk | yes — this is where concurrency pays |
| Push + PR create | GitHub secondary rate limit | **no** — ~1 content mutation/sec, fleet-wide |

For a 300-repo campaign the PR mutations alone cost ~5 minutes no matter what,
so clone speed dominates only for dry-runs and smaller fleets — but those are
the common cases (every campaign is dry-run first), so both matter.

**v0.1 — built in from the start:**

- **Two separate throttles, not one worker pool.** A clone/script pool
  (`--concurrency`, default 10 — safe to raise to 20–30, it's all local/network
  work) feeding a mutation throttle (token bucket, ~1/sec) that gates push + PR
  creation. Rationale: preemptive throttling beats reactive backoff — tripping a
  secondary rate limit costs a 60s+ penalty window, which is far slower than
  never tripping it. Lives in `campaign` (pool) and `client` (bucket).
- **Zero throttle events is the design goal, not graceful recovery.** The
  target for a normal run is that GitHub never returns a single 403/429.
  Concretely:
  - All mutations (PR create, and anything else content-creating) go through
    one global token bucket at 1/sec with jitter — GitHub's documented guidance
    is "wait at least one second between mutative requests", so we pace exactly
    to it rather than probing for the real ceiling.
  - Watch `x-ratelimit-remaining` on every response; if the primary budget
    (5,000/hr on a PAT) drops below a floor (~10% of what the rest of the run
    needs), pause and say so instead of running the token dry mid-campaign. A
    run's API cost is predictable up front — discovery pages + ~2 calls per
    changed repo — so `run` prints the estimate next to the repo count before
    starting.
  - If a secondary-limit 403 ever does arrive, it pauses **all** workers
    globally for the `Retry-After` window (not just the offending request) and
    halves the bucket rate for the rest of the run — continuing to fire from
    other goroutines during a penalty window is how accounts get escalating
    blocks. Backoff is the failsafe, never the pacing mechanism.
- **Minimal clones.** `--depth 1 --single-branch --no-tags` always. Typical
  service repo: 1–3s. 300 repos at concurrency 20 ≈ under a minute of clone time.
- **Sparse checkout for targeted edits** (`--sparse <path>`, repeatable): clone
  with `--filter=blob:none --sparse` then `git sparse-checkout set <paths>`, so
  only the blobs the script touches are fetched. For "bump the Dockerfile" this
  makes clone cost independent of repo size — the difference between seconds and
  minutes on monorepos. Not the default because the script contract promises a
  full working tree; it's an explicit opt-in that changes that promise.
- **Parallel discovery pagination.** Fetch page 1, read the last-page number
  from the `Link` header, then fetch remaining pages concurrently instead of
  walking them one by one. An org with 3,000 repos is 30 pages — 2s instead of 30s.
- **Measure before optimizing further.** Every `RepoResult` carries per-phase
  timings (clone / script / push / PR); the summary prints totals and the
  slowest repos, and `--output json` includes them. This tells us whether the
  next optimization should target clone or mutation — no guessing.

**v0.2 — when the fleet or repo size demands it:**

- **Persistent clone cache** (`~/.cache/goldfinger/<owner>/<repo>`): re-runs
  `git fetch --depth 1 && git reset --hard` instead of re-cloning. Turns the
  second dry-run of a campaign from minutes into seconds; needs eviction and
  corruption handling, hence not v0.1.
- **Code-search pre-filter** for content-based targeting: one search API query
  ("repos whose Dockerfile mentions image X") shrinks the clone set before any
  clone happens — the cheapest clone is the one you skip.
- **ETag-conditional discovery**: cache the org repo list; `304 Not Modified`
  responses are free against the rate limit and near-instant.
- **Maybe, if PR latency ever matters enough:** an API-edit fast path (Git Data
  API: blob → tree → commit → ref, no clone at all) for single-file transforms.
  It abandons the script contract, so it would be a separate opt-in mode —
  deliberately out until the timing data proves the need.

## Deliberately out (v0.2+)

- `status` / `merge` / `close` lifecycle commands
- `--repos-file`, `--language`, `--contains-file` targeting
- `--interactive` per-repo confirmation
- Fork-based PRs for repos without direct write access
- GitHub App auth, GHES base URL
