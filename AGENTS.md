# AGENTS.md

Guidance for AI agents working in this repository. (`CLAUDE.md` is a symlink to
this file, so Claude Code reads exactly the same content — one source of truth.)

## Two audiences — don't confuse them

- **Operating goldfinger** (running the CLI, in this repo or elsewhere): run
  `goldfinger guide` for the operator playbook, or read `cmd/guide.md`. Do **not**
  rely on this file for usage. One convention worth knowing up front: when you're
  developing a fleet change, `mirror --purpose <name>` (optionally `--branch
  <b>`) into a fresh, timestamped `~/goldfinger/<purpose>[-<branch>]-<stamp>`
  snapshot to read and test against — each run gets its own pristine dir,
  goldfinger never deletes it, so you clean it up when done. `mirror` prints that
  resolved workspace path as a bare line on stdout (banners/ghorg output go to
  stderr), so capture it rather than globbing for the stamped dir. One gotcha:
  `--branch` and `--clone-depth` are incompatible (a shallow clone only fetches
  each repo's default branch, so `--branch dev --clone-depth 1` would silently
  skip `dev` wherever it isn't the default) — goldfinger refuses the combo; omit
  `--clone-depth` when mirroring a non-default branch. Full recipe in
  `goldfinger guide`.
- **Changing goldfinger's code** (you are here): read `README.md` first for the
  product design and rationale, then follow the rules below and the build plan in
  `IMPLEMENTATION.md` — start at its "Start here" section; its pinned decisions
  and scope fences are binding.

## What goldfinger is

An orchestration layer. It resolves a repo selection (org/user + topic), freezes
it as a JSON lockfile, then delegates: **ghorg** mirrors the selection locally,
**multi-gitter** applies changes and opens PRs. goldfinger owns the *selection*;
it does not reimplement mirroring or PR-fanout.

## Hard rules

- goldfinger **never writes to GitHub and never runs `git` itself.** Discovery is
  read-only REST; mirroring is ghorg; commits/pushes/PRs are multi-gitter. Adding
  a `git` exec or a PR-create call means you're reinventing a delegated tool —
  stop.
- A real (non-dry-run) `goldfinger apply` opens PRs and must never happen on an
  agent's own initiative or by accident. `apply` defaults to dry-run; a real run
  additionally needs `--dry-run=false --confirm`. An agent may perform the real
  run **only when the human has explicitly authorized this specific fleet
  change** — and then must dry-run first, present the diff, and prefer `--draft`.
  Absent explicit authorization, the real run is the human's to execute.
- `apply` requires `--sign` on **every** run (`local` = the operator's own GPG
  key via the git binary, `github` = GitHub's web-flow key via the API, `none` =
  unsigned) — there is no default. An agent must pass it explicitly and state
  which mode it used and that mode's trust model when presenting a dry-run or a
  real run; never silently pick one.
- The selection lockfile is authoritative: `mirror` and `apply` read it and must
  never re-run discovery — so "the repos I mirror" and "the repos I change" are
  provably the same set, which is the whole product.
- Tokens go to child tools via their env vars (`GHORG_GITHUB_TOKEN`,
  `GITHUB_TOKEN`), never argv. Tests assert no token appears in argv.
- Flat packages (`cmd/`, `models/`, `client/`, `discovery/`, `selection/`,
  `mirror/`, `apply/`); no `internal/`/`pkg/`. Tests alongside code, testify.
- Go module deps are pinned (cobra, go-github, testify); adding one needs asking.
  ghorg and multi-gitter are runtime CLI deps invoked via `exec`, checked on PATH.

## Before every commit

`go build ./...`, `go vet ./...`, `go test ./...` clean — or just `make check`,
which mirrors CI's test job.
