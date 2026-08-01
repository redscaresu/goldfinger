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
- **One token, one UX** — you set `GOLD_FINGER_PAT`; goldfinger maps it to the env
  vars each tool expects (`GHORG_GITHUB_TOKEN`, `GITHUB_TOKEN`), checks both are
  installed, and frames their output.

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
export GOLD_FINGER_PAT=<PAT>   # one token; goldfinger maps it to ghorg + multi-gitter

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

- The command after `--` runs in each repo's checkout. If it changes files and
  exits 0, a PR is prepared.
- PR options: `--base-branch <main|dev>` (base the PR on a branch other than the
  default), `--pr-body`, `--label` / `--reviewer` (repeatable), `--draft`.
- **Safety:** `apply` defaults to `--dry-run` (shows the change, opens nothing). A
  real run requires **both** `--dry-run=false` **and** `--confirm` — the guard
  against an accidental fleet-wide PR blast. Opening real PRs is a human step.

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

Then set your token once:

```sh
export GOLD_FINGER_PAT=<a GitHub PAT with Contents + Pull requests read/write>
```

goldfinger maps `GOLD_FINGER_PAT` to the env vars ghorg and multi-gitter expect,
so you only set this one. Run `goldfinger guide` for the operator playbook.

## Requirements

- **Go** (to build goldfinger).
- **ghorg** and **multi-gitter** on `PATH`. goldfinger checks for them and prints
  install instructions if missing.
- A **git identity** (`git config user.name` / `user.email`) — multi-gitter
  authors the `apply` commit from it.
- **`GOLD_FINGER_PAT`** — a GitHub PAT. goldfinger uses it for API discovery and
  maps it to the env vars ghorg (`GHORG_GITHUB_TOKEN`) and multi-gitter
  (`GITHUB_TOKEN`) expect, so you set one token, not three.

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
