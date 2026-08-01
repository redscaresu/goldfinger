# goldfinger

CLI for SREs to fan out changes across hundreds of GitHub repos: discover by
org/topic, clone, run a script per repo, open PRs — without burning GraphQL
rate limits.

- `README.md` — product design and rationale. Read first.
- `IMPLEMENTATION.md` — the implementation plan. **If you are building this
  tool, start at its "Start here" section and follow the build order.** Its
  pinned decisions and scope fences are binding.

## Hard rules

- Never execute a non-dry-run `goldfinger run` (anything that pushes or opens
  PRs) against real repos — that step is always run by the human.
- Never print, log, or commit a GitHub token; `gitexec` output must redact it
  (there is a required test for this).
- Flat package structure (`cmd/`, `models/`, `client/`, `discovery/`,
  `gitexec/`, `campaign/`) — no `internal/`, no `pkg/`. Tests live alongside
  code and use testify.
- Dependencies are pinned in IMPLEMENTATION.md's handoff notes: cobra,
  go-github, errgroup, x/time/rate, testify. Adding anything else requires
  asking first.
- `go build ./...`, `go vet ./...`, `go test ./...` all clean before every
  commit.
