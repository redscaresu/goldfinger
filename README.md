# goldfinger

[![CI](https://github.com/redscaresu/goldfinger/actions/workflows/ci.yml/badge.svg)](https://github.com/redscaresu/goldfinger/actions/workflows/ci.yml)

An orchestration layer for fleet-wide GitHub work. goldfinger resolves a set of
repos once — by org/user and topic — freezes it as a reviewable **selection**,
then drives two best-in-class tools against that exact set:

- **[ghorg](https://github.com/gabrie30/ghorg)** to mirror the selection into a
  persistent local workspace (clone/pull hundreds of repos, kept fresh).
- **[multi-gitter](https://github.com/lindell/multi-gitter)** to apply a change
  across the selection and open PRs.

The value goldfinger adds is not mirroring or PR-fanout — those tools already do
each well. It's that **"the repos I mirror" and "the repos I change" are provably
the same set**, captured in one artifact you can inspect before anything runs.

## It's a wrapper over ghorg and multi-gitter

goldfinger does **not** clone repos or open PRs itself. It is a thin wrapper that
shells out to two existing, mature CLI tools and coordinates them around one
shared selection:

| You run | goldfinger shells out to | which does the work |
|---|---|---|
| `goldfinger mirror` | `ghorg clone <owner> --target-repos-path=<names> --path=<ws>` | clone/pull the repos locally |
| `goldfinger apply … -- <cmd>` | `multi-gitter run <script> --repo a/b --repo a/c …` | run the change + open PRs |

Everything goldfinger *itself* does is the glue around those two calls:

- **Resolve the selection** — one GitHub API pass turns `--org` / `--topic` into a
  concrete `owner/name` list, frozen in a lockfile.
- **Feed both tools the identical set** — ghorg via a `--target-repos-path` names
  file, multi-gitter via repeated `--repo` flags. Neither tool re-discovers, so
  the two phases can't drift apart.
- **No token to set up** — if you're already logged in with the GitHub CLI,
  goldfinger uses that session automatically (`gh auth token`); nothing to export.
  It maps that one token to the env vars each tool expects
  (`GHORG_GITHUB_TOKEN`, `GITHUB_TOKEN`), checks both tools are installed, and
  frames their output. `GOLD_FINGER_PAT` is the fallback for when there's no local
  gh login (e.g. CI).

So goldfinger is a few hundred lines of orchestration, not a reimplementation.
Rebuilding ghorg or multi-gitter would at best match tools that are already fast
(both Go, both shell out to `git`, both do bounded-concurrency clones) — the win
is making them share one reviewable selection.

## Why this exists

Fleet changes (bump a base image, patch a dependency, rotate a CI config) have
two phases that are usually done with different, disconnected tooling:

1. **Get the repos locally** so you can grep, open them in an editor, and figure
   out *what* needs changing and *where*. (People hand-roll clone loops, or use
   ghorg.)
2. **Apply the change and open PRs.** (People hand-roll scripts, or use
   multi-gitter.)

The gap: the set you *explored* in phase 1 and the set you *changed* in phase 2
are computed separately and drift apart — different filters, different moments in
time, a repo added or retopic'd in between. goldfinger closes that gap by making
the selection a single frozen artifact that feeds both phases.

## The model

```
                 ┌──────────────────────────────┐
   select  ───▶  │  goldfinger.selection  (lock) │  owner/name list + provenance
                 └──────────────────────────────┘
                        │                    │
             mirror ────┘                    └──── apply
          (ghorg clone/pull            (multi-gitter run:
           the exact set into           script + PR across
           a local workspace)           the exact set)
```

## Quickstart

```sh
# No token setup needed if you're already logged in with the GitHub CLI —
# goldfinger picks up your `gh auth` session automatically and maps it to
# ghorg + multi-gitter. (Log in once with: gh auth login)
#
# Only if you have no local gh login (e.g. CI), set a PAT explicitly — it
# overrides the gh session when present:
# export GOLD_FINGER_PAT=<a GitHub PAT with Contents + Pull requests read/write>

# 1. freeze the target set -> ./goldfinger.selection
goldfinger select --org mycompany --topic platform

# 2. clone/pull them locally to grep and inspect (optional)
goldfinger mirror

# 3. dry-run the change — shows the diff, opens nothing
goldfinger apply --branch bump-go --commit-message "Bump Go" --pr-title "Bump Go" \
  -- sed -i 's|golang:1.22|golang:1.24|g' Dockerfile

# 4. for real — opens the PRs (requires both flags)
goldfinger apply --branch bump-go --commit-message "Bump Go" --pr-title "Bump Go" \
  --dry-run=false --confirm -- sed -i 's|golang:1.22|golang:1.24|g' Dockerfile
```

`goldfinger guide` prints this playbook from the binary itself.

### `goldfinger select`

Resolves the repo set via the GitHub API and writes the selection lockfile.

```sh
goldfinger select --org mycompany --topic platform --topic payments
# or every non-archived repo:
goldfinger select --org mycompany --all-repos
```

- `--org <owner>` — a GitHub org **or** user (required).
- `--all-repos` or `--topic <t>` (repeatable, any-match) — exactly one is
  required. Topic filtering is goldfinger's, applied here and frozen into the
  lockfile; ghorg's and multi-gitter's own topic flags are bypassed.
- Writes `goldfinger.selection` and prints the set for review. The lockfile is
  plain JSON — inspect or diff it before mirroring/applying:

```json
{
  "version": 1,
  "owner": "mycompany",
  "ownerType": "Organization",
  "filter": { "topics": ["platform"] },
  "resolvedAt": "2026-08-01T15:17:53Z",
  "tool": "goldfinger v0.1.0",
  "repos": [
    { "owner": "mycompany", "name": "billing", "cloneURL": "https://github.com/mycompany/billing.git", "defaultBranch": "main", "topics": ["platform"] }
  ]
}
```

### `goldfinger mirror`

Reads the lockfile and mirrors exactly that set locally via ghorg.

```sh
goldfinger mirror
```

Shells out to `ghorg clone <owner> --target-repos-path=<names> --path=<workspace>`
so ghorg clones/pulls only the selected repos into goldfinger's workspace. Re-run
any time to refresh; ghorg pulls existing clones instead of re-cloning.

By default a re-sync also `git clean`s each clone, so the workspace stays a
pristine reflection of upstream (local edits are discarded). Pass `--no-clean`
to preserve local changes across re-syncs — i.e. treat the mirror as an editable
workspace rather than a read-only reflection. Other passthroughs: `--concurrency`,
`--clone-depth` (shallow), `--dry-run`.

To keep the lockfile authoritative, goldfinger neutralises ambient ghorg config
that could change the set behind your back: it strips set-narrowing/pruning
`GHORG_*` environment variables (`GHORG_TOPICS`, `GHORG_MATCH_REGEX`,
`GHORG_SKIP_ARCHIVED`, `GHORG_PRUNE*`, …) from ghorg's environment and forces an
empty `--ghorgignore-path`, so a stray env var or `~/.config/ghorg/ghorgignore`
can't silently drop repos.

### `goldfinger apply`

Reads the lockfile and runs a change across exactly that set via multi-gitter.

```sh
goldfinger apply --branch bump-go-1.24 \
  --commit-message "Bump golang base image to 1.24" \
  --pr-title "Bump golang base image to 1.24" \
  -- sed -i 's|golang:1.22|golang:1.24|g' Dockerfile
```

Shells out to `multi-gitter run`, passing one `--repo owner/name` per lockfile
entry plus the script and PR flags.

- The command after `--` runs in each repo's checkout, **on your machine** — so
  it must be portable to your OS. `sed -i 's|…|…|g'` is a GNU-ism that fails on
  macOS (BSD `sed` needs `sed -i ''`). For anything non-trivial, or edits that
  differ per file, pass a script instead of an inline command — e.g.
  `-- python3 /abs/path/migrate.py` — which is portable and can carry per-file
  logic a single `sed` can't.
- **Base branch.** With `--base-branch` omitted, each PR targets that repo's
  **own default branch** — so a mixed `dev`/`main` selection routes correctly
  per repo with no extra flags. Pass `--base-branch <name>` to force one branch
  across the whole set (note: it's a single global value — if some repos default
  to `main` but you want `dev` there specifically, split the selection instead).
- Other PR options: `--pr-body` (or `--pr-body-file <path>` to load a long body
  from a file — the two are mutually exclusive), `--label` / `--reviewer`
  (repeatable), `--draft`.
- **Safety:** `apply` defaults to `--dry-run` (shows the change, opens nothing). A
  real run requires **both** `--dry-run=false` **and** `--confirm` — the guard
  against an accidental fleet-wide PR blast. A real run should always follow a
  reviewed dry-run; when an agent runs it under explicit human authorization,
  prefer `--draft`.
- **Config isolation:** goldfinger invokes multi-gitter with an empty `--config`
  so an ambient `multi-gitter` config file can't inject its own repo/org
  selection or filters — the lockfile's `--repo` set is the only source of truth
  for which repos get changed.
- **Rate limits.** Opening PRs across a large fleet can trip GitHub's *secondary*
  rate limits — separate from the 5,000/hr REST budget. GitHub allows **80
  content-generating requests per minute** and **500 per hour**, and a single PR
  is more than one such request: the PR itself, plus one each for `--label` and
  `--reviewer`. So a PR with labels + reviewers costs ~3–4, putting the real
  ceiling around ~150 PRs/hour (or ~500 with none).
  - `--batch-size N` opens PRs in chunks of `N` repos, and `--batch-pause D`
    sleeps `D` (e.g. `90s`) between chunks. This keeps you under the **80/min**
    limit — size a batch so `N × requests-per-PR` stays well below 80 and pause
    ≥ `60s`. Example: `--batch-size 15 --batch-pause 90s`.
  - Batching **cannot** beat the **500/hour** ceiling — no within-hour pacing
    can. For a fleet past that, the run will eventually hit the hourly limit and
    error; just **re-run the same `apply`** after the hour resets. multi-gitter's
    default `conflict-strategy: skip` means repos that already have a branch/PR
    are skipped, so a re-run only attempts the remainder — apply is naturally
    resumable, which is how you spread a big fleet across hours.

### Named selections

By default a selection lives in `./goldfinger.selection`. To keep several
standing cohorts, give each a **name** — stored in a registry
(`~/.config/goldfinger/selections/<name>.json`) and referred to by `--name` on
any command:

```sh
goldfinger select --name platform --org acme --topic platform   # define / refresh
goldfinger select --name payments --org acme --topic payments
goldfinger selections                                           # list them
goldfinger mirror --name platform                               # operate by name
goldfinger apply  --name payments -- sed -i '…' Dockerfile
```

`--name` and `--selection <path>` are mutually exclusive; re-running
`select --name X` refreshes that cohort in place.

## Install

goldfinger itself:

```sh
# from source
go install github.com/redscaresu/goldfinger/cmd@latest   # installs `goldfinger`
# or build the repo
git clone https://github.com/redscaresu/goldfinger && cd goldfinger && make build  # -> bin/goldfinger
```

Release builds for linux/darwin × amd64/arm64 are attached to each tagged
release on GitHub.

The two tools goldfinger drives (both Go, install with `go install`):

```sh
go install github.com/gabrie30/ghorg@latest
go install github.com/lindell/multi-gitter@latest
# ensure your GOPATH bin is on PATH, e.g.:
export PATH="$PATH:$(go env GOPATH)/bin"
```

For auth, if you already use the GitHub CLI there's **nothing to set up** —
goldfinger picks up your `gh auth` session automatically (it shells out to
`gh auth token`). Log in once and you're done:

```sh
gh auth login   # grants `repo` scope by default, which is what goldfinger needs
```

`GOLD_FINGER_PAT` is the **fallback** for when no local gh login is available:
you don't have the gh CLI set up, or you're running in **CI** (no interactive
login — store the PAT as a secret and export it in the job). When set, it takes
precedence over the gh session. It needs Contents + Pull requests read/write:

```sh
export GOLD_FINGER_PAT=<a GitHub PAT with Contents + Pull requests read/write>
```

Either way goldfinger resolves a single token and maps it to the env vars ghorg
and multi-gitter expect, so you never juggle three. Run `goldfinger guide` for
the operator playbook.

## Requirements

- **Go** (to build goldfinger).
- **ghorg** and **multi-gitter** on `PATH`. goldfinger checks for them and prints
  install instructions if missing.
- A **git identity** (`git config user.name` / `user.email`) — multi-gitter
  authors the `apply` commit from it.
- **A GitHub token** for API discovery, mapped to the env vars ghorg
  (`GHORG_GITHUB_TOKEN`) and multi-gitter (`GITHUB_TOKEN`) expect, so you set one
  token, not three. By default goldfinger reuses your local `gh auth login`
  session automatically — nothing to set. Setting `GOLD_FINGER_PAT` overrides
  that and is the fallback when you have no local gh login or you're running in
  CI.

## For AI agents

goldfinger is mostly operated by AI agents. `goldfinger guide` prints a compact
operator playbook (the workflow, the dry-run-by-default safety rule, examples)
that travels with the binary. The selection lockfile is JSON, and every error
names the next action — so an agent can self-orient and recover without this
README. Contributor-agent rules live in `AGENTS.md` and `CLAUDE.md`.

## Development

```sh
make check   # go vet + race tests (mirrors CI's test job)
make e2e     # full-pipeline test against a sandbox repo (needs GOLD_FINGER_PAT + gh)
make hooks   # install the gitleaks pre-commit hook
```

CI runs the unit tests, gitleaks, govulncheck, and an end-to-end job that opens
and tears down a real PR on a sandbox repo. Contributor rules are in `AGENTS.md`
and `CLAUDE.md`.

## Design docs

- `IMPLEMENTATION.md` — the build plan: package layout, the selection format, the
  ghorg/multi-gitter handoffs, build order, and pinned decisions.

## Non-goals (v0.1)

- No reimplementation of clone/pull or PR machinery (delegated by design).
- github.com only; GHES is a later base-URL change.
- No long-running service — the lockfile is the only persisted state, and
  `mirror`/`apply` recompute nothing.
