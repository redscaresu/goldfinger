# goldfinger — Implementation Plan (v0.1)

## Start here (for a fresh implementing agent)

You are implementing this plan from scratch. Everything you need is in this repo:

1. Read `README.md` first — it's the *why* and the product shape. goldfinger is
   an **orchestration layer**: it resolves a repo selection, then delegates
   mirroring to **ghorg** and change-application to **multi-gitter**. It does not
   reimplement either.
2. This file is the *what and how*. Where it and the README differ on detail,
   this file wins.
3. Work the **Build order** steps in sequence. One step = one commit (or a few
   small ones); make each step's acceptance criteria pass before the next.
4. Things only the human can provide — stop and ask, don't work around:
   - a `GOLD_FINGER_PAT` PAT for live testing (exported in their shell, never
     pasted into chat)
   - execution of any real (non-dry-run) `goldfinger apply` — that opens PRs
5. Live *read-only* checks you may run yourself once a token is in the env:
   `goldfinger select` against a real owner, and `goldfinger mirror` (cloning is
   read-only w.r.t. GitHub). Anything that opens a PR is the human's to run.
6. `go build ./...`, `go vet ./...`, `go test ./...` clean before every commit;
   `make check` mirrors CI.

## Scope (v0.1)

- Auth: one PAT in `GOLD_FINGER_PAT`. goldfinger uses it for API discovery and
  exports it to child tools as `GHORG_GITHUB_TOKEN` and `GITHUB_TOKEN`.
- Target: one owner (org **or** user) via `--org`. Selection is **exactly one
  of** `--all-repos` or `--topic <t>` (repeatable, any-match).
- Commands: `select` (resolve + write lockfile), `mirror` (ghorg), `apply`
  (multi-gitter). Nothing else.
- goldfinger owns the **selection**; ghorg owns the **mirror**; multi-gitter owns
  the **apply**.

## CLI surface

```
goldfinger select --org <owner> (--all-repos | --topic <t> [--topic <t>...])
                  [--selection <path>]   # default ./goldfinger.selection

goldfinger mirror [--selection <path>]
                  [--workspace <dir>]    # default ~/goldfinger/<owner>
                  [--concurrency N]      # passthrough to ghorg
                  [--clone-depth N]      # passthrough to ghorg (shallow)

goldfinger apply  [--selection <path>]
                  --branch <name>
                  --commit-message <msg>
                  --pr-title <title>
                  [--pr-body <body>]
                  [--label <l>...] [--reviewer <r>...] [--draft]
                  [--dry-run]            # DEFAULT true; must pass --dry-run=false to open PRs
                  -- <command> [args...] # the per-repo script for multi-gitter
```

Flag rules (enforced in `cmd/` before any external call):

- `GOLD_FINGER_PAT` must be non-empty; fail fast.
- `select`: `--org` required; exactly one of `--all-repos` / `--topic`.
- `mirror` / `apply`: the selection file must exist and parse (tell the user to
  run `select` first if not).
- `apply`: `--branch`, `--commit-message`, `--pr-title`, and a script after `--`.

## Package layout

Flat top-level packages per [simpleAPI](https://github.com/redscaresu/simpleAPI),
tests alongside code:

```
cmd/         main.go — cobra wiring, flag validation, tool-presence checks
models/      Repo, Selection, SelectionFilter, ApplySpec
client/      GitHub API discovery (go-github): ListRepos  [BUILT]
discovery/   Select — pure topic/archived filtering        [BUILT]
selection/   read/write the selection lockfile (JSON)
mirror/      ghorg shell-out wrapper: build args, exec, map token
apply/       multi-gitter shell-out wrapper: build args, exec, map token
```

Dropped from earlier drafts (they were reinventing the delegated tools):
`gitexec`, `campaign`, and `client.CreatePR`. Do not build them.

### models

```go
type Repo struct {
    Owner, Name, CloneURL, DefaultBranch string
    Topics   []string
    Archived bool
}
func (r Repo) FullName() string // "owner/name"

type SelectionFilter struct {
    AllRepos bool
    Topics   []string
}

type Selection struct {
    Version    int             // schema version, start at 1
    Owner      string          // org or user login
    OwnerType  string          // "User" | "Organization"
    Filter     SelectionFilter
    ResolvedAt time.Time
    Tool       string          // e.g. "goldfinger dev"
    Repos      []Repo
}

type ApplySpec struct {
    Branch, CommitMessage, PRTitle, PRBody string
    Labels, Reviewers []string
    Draft, DryRun     bool
    Script            []string
}
```

### client  [BUILT]

`go-github` (v89) wrapper. `New(token) (*Client, error)`, `Verify(ctx) (login,
error)` (cheap `GET /user`, fail fast on bad token), and `ListRepos(ctx, owner)`
which dispatches on owner type: authenticated user → `/user/repos`
(`affiliation=owner`, includes private); other user → `/users/{u}/repos`; org →
`/orgs/{o}/repos`. Paginates via `Response.NextPage`. Topics come back in the
listing (preview header is set by go-github). Returns `[]models.Repo`.

### discovery  [BUILT]

`Select(repos, Filter) []models.Repo` — excludes archived always; includes a repo
if `AllRepos` or it carries any requested topic. Pure, table-tested.

### selection

```go
func Write(path string, s models.Selection) error   // JSON, 0644, atomic write
func Read(path string) (models.Selection, error)     // parse + validate Version
```

The lockfile is the shared artifact. JSON so it's reviewable and round-trips in a
test. `Read` errors clearly if the file is missing ("run `goldfinger select`
first") or the schema version is unknown.

### mirror

```go
func Mirror(ctx, s models.Selection, workspace, token string, opts Options) error
```

- Writes the repo **names** (basename only — ghorg matches on name) to a temp
  file; `defer` its removal.
- Execs `ghorg clone <s.Owner> --target-repos-path=<tmp> --path=<workspace>
  --token=<token>` plus optional `--concurrency` / `--clone-depth`. For a user
  target, pass ghorg's user flag/type as needed (see step 3 to confirm the exact
  invocation against real ghorg).
- Streams ghorg's stdout/stderr through. Wrap a non-zero exit with context. The
  token is passed via the child env (`GHORG_GITHUB_TOKEN`), not the command line,
  so it never lands in process listings or our error text.

### apply

```go
func Apply(ctx, s models.Selection, spec models.ApplySpec, token string) error
```

- Builds `multi-gitter run <script...>` with one `--repo owner/name` per
  `s.Repos`, plus `--branch`, `--commit-message`, `--pr-title`, and any
  `--pr-body` / `--label` / `--reviewer` / `--draft`. Adds `--dry-run` when
  `spec.DryRun`.
- Token via child env `GITHUB_TOKEN`.
- **Scale caveat:** multi-gitter has no repo-list-file input, so the set is
  passed as repeated `--repo` flags. Fine for hundreds; if `len(s.Repos)` exceeds
  a safe threshold (~1000), refuse with a clear message rather than risk an
  `E2BIG` command line. Log the count either way — no silent truncation.

### cmd

- cobra root + `select` / `mirror` / `apply`. All flag validation up front.
- `mirror` and `apply` verify the child tool is on `PATH` (`exec.LookPath`)
  before doing anything, and print an install hint if absent.
- One-token mapping: read `GOLD_FINGER_PAT`; pass it to `client` and to the child
  env vars. The user never sets `GHORG_GITHUB_TOKEN` or `GITHUB_TOKEN`.
- `apply` prints the repo count and the resolved-at time from the lockfile, and
  refuses a real run (`--dry-run=false`) without an explicit confirmation flag —
  the guard against an accidental fleet-wide PR blast.

## Auth, identity, tokens

- **PAT scopes.** Classic `repo`, or fine-grained Metadata (read) + Contents
  (read/write, for multi-gitter's push) + Pull requests (read/write) on the
  target repos.
- **One token in, three uses out.** `GOLD_FINGER_PAT` → goldfinger's API client,
  → `GHORG_GITHUB_TOKEN` for ghorg, → `GITHUB_TOKEN` for multi-gitter. Always via
  child env, never argv.
- **Commit identity & signing** are multi-gitter's concern (it does the commits).
  goldfinger does not commit.

## CI/CD  [BUILT in step 1]

`.github/workflows/ci.yml` (test + gitleaks + govulncheck + main-branch
build-binary), `release.yml`, `.gitleaks.toml`, `.github/dependabot.yml`,
`SECURITY.md`, `Makefile` (`build`/`test`/`check`/`hooks`), `scripts/pre-commit`.
CI runs `go vet` and `go test -race -count=2 ./...`. These exist and are green;
keep them passing.

## Build order

1. **Skeleton + CI/CD** — module, `models`, cobra skeleton, all CI/CD machinery.
   **[DONE, CI green.]** (Command names predate the pivot; step 2 renames them.)
2. **select** — wire `client` + `discovery` + `selection` into
   `goldfinger select`: resolve via API, write the lockfile, print the set.
   `client` and `discovery` are already built and tested; add `selection`
   (write/read + round-trip test) and the `select` command. Deliverable:
   `goldfinger select --org redscaresu --all-repos` writes a lockfile listing
   redscaresu's repos. First live read-only validation.
3. **mirror** — `mirror` package + command: read lockfile → temp names file →
   exec ghorg into the workspace. Confirm the exact ghorg invocation (org vs
   user, flags) against real ghorg. Deliverable: `goldfinger mirror` clones the
   selected repos locally. Read-only w.r.t. GitHub, safe to run.
4. **apply (dry-run first)** — `apply` package + command: read lockfile → exec
   multi-gitter with `--repo` list + script + `--dry-run`. Deliverable: a dry-run
   apply shows multi-gitter's planned change across the set. The real
   PR-opening run is **the human's** to execute and confirm.
5. **Hardening + AI-facing docs** — token-redaction audit across both
   shell-outs, tool-presence errors, lockfile schema-version handling,
   `--dry-run=false` confirmation guard, README usage verified end-to-end. Plus,
   because goldfinger is mostly AI-operated: a `goldfinger guide` command that
   prints a `go:embed`ded operator playbook (select→mirror→apply, the
   dry-run-by-default safety rule, copy-paste examples) — reachable at runtime by
   any consuming agent, wherever it runs — and an `AGENTS.md` for contributor
   agents (complements the repo `CLAUDE.md`). The JSON lockfile and
   next-action error messages are already agent-legible; keep them so.

Dependencies: `spf13/cobra`, `google/go-github/v89`, `stretchr/testify`. Plus two
**runtime** CLI dependencies invoked via `exec`: **ghorg** and **multi-gitter**
(not Go module deps). No other Go modules without asking.

## Builder handoff notes

**Pinned — do not re-litigate:**

- Module `github.com/redscaresu/goldfinger`. Env var `GOLD_FINGER_PAT`.
- goldfinger never talks to the GitHub *write* API and never runs `git` itself.
  Discovery is read-only REST; mirroring is ghorg; commits/pushes/PRs are
  multi-gitter. If you find yourself adding `git` exec or a PR-create call, stop —
  that's a delegated concern.
- The selection lockfile is authoritative. `mirror` and `apply` must operate on
  the lockfile's repo list, never re-run discovery. This is the core guarantee.
- Child tools get the token via env, never argv. A test must assert the built
  argv for mirror/apply contains no token substring.
- Shell-out wrappers take an `exec` seam (e.g. a `func(ctx, name, args, env)
  error` field, defaulting to a real runner) so command construction is
  unit-testable without ghorg/multi-gitter installed. This is the one allowed
  abstraction (it has a real second implementation: the test fake).

**Per-step acceptance criteria:**

2. `goldfinger select --org <owner> --topic x` writes a parseable lockfile and
   prints `owner/name` lines. `selection.Write`→`Read` round-trips in a test;
   `client` pagination (httptest, 3 pages) and `discovery` filtering already
   covered.
3. `mirror` builds the correct ghorg argv (unit test via the exec seam, asserting
   `--target-repos-path` points at a file of the selected names and no token in
   argv) and, live, clones the selected repos into the workspace.
4. `apply` builds the correct multi-gitter argv (unit test: one `--repo` per
   selected repo, `--dry-run` present by default, no token in argv). A live
   dry-run runs clean. Real run deferred to the human.
5. Token-redaction test across both wrappers; missing-tool produces a one-line
   install hint; `go vet` + `go test ./...` clean.

**Guardrails:**

- No config files; the only state is the lockfile. No env vars beyond
  `GOLD_FINGER_PAT` for the user to set.
- No new Go module deps beyond the three above without asking.
- If a capability seems missing, stop and ask — don't invent surface area or
  reach back into the dropped clone/PR engine.
- If a step's acceptance criteria can't be met as written, stop and report.

## Deliberately out (v0.2+)

- Named/multiple selections and a workspace registry (ghorg's `reclone.yaml`
  territory) — v0.1 is one selection file at a time.
- `--repo-file` / code-search targeting; GHES base URL; GitHub App auth.
- A `status` command diffing the lockfile against the live org or the mirror.
- Fork-based PRs (multi-gitter's concern if ever needed).
