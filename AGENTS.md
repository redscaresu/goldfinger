# AGENTS.md

Guidance for AI agents working in this repository. (Claude Code also reads
`CLAUDE.md`, which carries the same rules.)

## Two audiences — don't confuse them

- **Operating goldfinger** (running the CLI, in this repo or elsewhere): run
  `goldfinger guide` for the operator playbook, or read `cmd/guide.md`. Do **not**
  rely on this file for usage.
- **Changing goldfinger's code** (you are here): follow the rules below and the
  build plan in `IMPLEMENTATION.md`.

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
- Never run a real (non-dry-run) `goldfinger apply` against real repos. `apply`
  defaults to dry-run; a real run needs `--dry-run=false --confirm` and is the
  human's to execute.
- The selection lockfile is authoritative: `mirror` and `apply` read it and must
  never re-run discovery.
- Tokens go to child tools via their env vars (`GHORG_GITHUB_TOKEN`,
  `GITHUB_TOKEN`), never argv. Tests assert no token appears in argv.
- Flat packages (`cmd/`, `models/`, `client/`, `discovery/`, `selection/`,
  `mirror/`, `apply/`); no `internal/`/`pkg/`. Tests alongside code, testify.
- Go module deps are pinned (cobra, go-github, testify); adding one needs asking.
  ghorg and multi-gitter are runtime CLI deps invoked via `exec`, checked on PATH.

## Before every commit

`go build ./...`, `go vet ./...`, `go test ./...` clean — or just `make check`,
which mirrors CI's test job.
