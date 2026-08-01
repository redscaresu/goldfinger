# goldfinger

An orchestration layer for fleet-wide GitHub work. goldfinger resolves a repo
selection (by org/user + topic), freezes it as a reviewable lockfile, then
delegates: **ghorg** mirrors the selection locally, **multi-gitter** applies a
change and opens PRs across it. goldfinger's job is the *shared selection*, not
mirroring or PR-fanout.

- `README.md` — product design and rationale. Read first.
- `IMPLEMENTATION.md` — the build plan. **If you are building this tool, start at
  its "Start here" section and follow the build order.** Its pinned decisions and
  scope fences are binding.

## Hard rules

- goldfinger **never writes to GitHub and never runs `git` itself.** Discovery is
  read-only REST; mirroring is delegated to ghorg; commits/pushes/PRs are
  delegated to multi-gitter. Adding a `git` exec or a PR-create call is out of
  scope — it means you're reinventing a delegated tool.
- A real (non-dry-run) `goldfinger apply` opens PRs via multi-gitter and must
  never happen on an agent's own initiative or by accident — hence `apply`
  defaults to dry-run and a real run additionally requires `--confirm`. An agent
  **may** perform the real run, but only when the human has explicitly authorized
  this specific fleet change; even then it must (1) run the dry-run first and
  present the diff, (2) prefer `--draft` so PRs open not-ready-for-review, and
  (3) pass `--dry-run=false --confirm`. Absent explicit authorization, the real
  run is the human's to execute.
- The selection lockfile is authoritative: `mirror` and `apply` operate on it and
  must never re-run discovery. "The repos I mirror" and "the repos I change" are
  provably the same set — that guarantee is the whole product.
- Never print, log, or commit a token. `GOLD_FINGER_PAT` is passed to child tools
  (ghorg, multi-gitter) via their env vars (`GHORG_GITHUB_TOKEN`,
  `GITHUB_TOKEN`), never on the command line. A test asserts no token in argv.
- Flat package structure (`cmd/`, `models/`, `client/`, `discovery/`,
  `selection/`, `mirror/`, `apply/`) — no `internal/`, no `pkg/`. Tests live
  alongside code and use testify.
- Go module deps are pinned: cobra, go-github, testify. Adding another requires
  asking first. ghorg and multi-gitter are **runtime** CLI dependencies invoked
  via `exec` (not Go modules); goldfinger checks they are on `PATH`.
- `go build ./...`, `go vet ./...`, `go test ./...` all clean before every
  commit. `make check` mirrors CI's test job exactly.
