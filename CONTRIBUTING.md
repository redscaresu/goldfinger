# Contributing to goldfinger

Thanks for your interest in improving goldfinger. This file is the short,
conventional entry point; the detailed engineering rules live in
[`AGENTS.md`](./AGENTS.md) (which `CLAUDE.md` symlinks to — one source of truth).

## How to contribute

- **Bugs / features:** open a [GitHub issue](https://github.com/redscaresu/goldfinger/issues).
- **Changes:** fork, branch, and open a pull request against `main`. PRs run the
  full CI suite (tests, lint, secret scan, vuln scan, CodeQL, e2e) and must be
  green before review.
- **Security issues:** do **not** open a public issue — follow
  [`SECURITY.md`](./SECURITY.md) (private vulnerability reporting).

## Before you push

Run the same gates CI runs:

```sh
make check   # go build + go test + golangci-lint (gosec + staticcheck)
```

`go build ./...`, `go vet ./...`, and `go test ./...` must all be clean, and
coverage must stay at or above the 80% floor.

## What to read first

- [`README.md`](./README.md) — product design, rationale, and full CLI usage.
- [`AGENTS.md`](./AGENTS.md) — the **Hard rules** (goldfinger never writes to
  GitHub and never runs `git` itself; tokens go to child tools via env, never
  argv; the selection lockfile is authoritative), package layout, and the
  pre-commit checklist. New functionality is expected to come with tests.

By contributing you agree that your contributions are licensed under the
project's [Apache-2.0](./LICENSE) license.
