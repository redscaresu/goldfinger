# goldfinger

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
- Writes `goldfinger.selection` (owner/name list plus provenance: owner, filters,
  resolved-at, tool version) and prints the set for review.

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
entry plus the script and PR flags. Defaults to `--dry-run`; opening real PRs is
an explicit, human-run step.

## What goldfinger does and doesn't own

| Concern | Owner |
|---|---|
| Resolving the repo set (org/user + topic) | **goldfinger** (GitHub API) |
| The frozen, reviewable selection artifact | **goldfinger** |
| One-token UX, dependency checks, wiring | **goldfinger** |
| Cloning/pulling the mirror, keeping it fresh | ghorg |
| Clone→script→commit→push→PR for the apply | multi-gitter |

goldfinger deliberately does **not** reimplement mirroring or PR-fanout. ghorg
and multi-gitter are mature and fast (both Go, both shell out to `git`, both do
bounded-concurrency clones); rebuilding them would at best match them. goldfinger
is the thin, opinionated glue that makes them share one selection.

## Requirements

- **Go** (to build goldfinger).
- **ghorg** and **multi-gitter** on `PATH`. goldfinger checks for them and prints
  install instructions if missing.
- **`GOLD_FINGER_PAT`** — a GitHub PAT. goldfinger uses it for API discovery and
  maps it to the env vars ghorg (`GHORG_GITHUB_TOKEN`) and multi-gitter
  (`GITHUB_TOKEN`) expect, so you set one token, not three.

## Design docs

- `IMPLEMENTATION.md` — the build plan: package layout, the selection format, the
  ghorg/multi-gitter handoffs, build order, and pinned decisions.

## Non-goals (v0.1)

- No reimplementation of clone/pull or PR machinery (delegated by design).
- github.com only; GHES is a later base-URL change.
- No long-running service — the lockfile is the only persisted state, and
  `mirror`/`apply` recompute nothing.
