# goldfinger

[![CI](https://github.com/redscaresu/goldfinger/actions/workflows/ci.yml/badge.svg)](https://github.com/redscaresu/goldfinger/actions/workflows/ci.yml)
[![coverage](https://raw.githubusercontent.com/redscaresu/goldfinger/badges/coverage.svg)](https://github.com/redscaresu/goldfinger/actions/workflows/ci.yml)
[![zizmor](https://github.com/redscaresu/goldfinger/actions/workflows/zizmor.yml/badge.svg)](https://github.com/redscaresu/goldfinger/actions/workflows/zizmor.yml)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/redscaresu/goldfinger/badge)](https://scorecard.dev/viewer/?uri=github.com/redscaresu/goldfinger)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/13997/badge)](https://www.bestpractices.dev/projects/13997)
[![SLSA 3](https://img.shields.io/badge/SLSA-Level_3-green)](https://slsa.dev/spec/v1.0/levels#build-l3)
[![Release](https://img.shields.io/github/v/release/redscaresu/goldfinger)](https://github.com/redscaresu/goldfinger/releases/latest)
[![License](https://img.shields.io/github/license/redscaresu/goldfinger)](LICENSE)

**Fleet-wide GitHub changes, made cheap, rate-limit-safe, and reviewable.**
goldfinger resolves a set of repos once — by org/user and topic — freezes it as a
reviewable **selection** lockfile, then drives two mature tools against that exact
set: **[ghorg](https://github.com/gabrie30/ghorg)** to mirror the repos locally,
**[multi-gitter](https://github.com/lindell/multi-gitter)** to apply a change and
open the PRs. It clones nothing and opens no PRs itself. It's **built to be driven
by AI agents as much as by people**.

## Demo

```console
# 1. freeze the target set → ./goldfinger.selection
$ goldfinger select --org mycompany --topic platform
✓ 24 repo(s) written to ./goldfinger.selection (digest a1b2c3d4)

# 2. clone them locally to grep and test against (no API reads)
$ goldfinger mirror
✓ mirror complete → ~/goldfinger/mycompany

# 3. read the fleet — regex over the local clones, zero GitHub rate limit
$ goldfinger scan "golang:1.22"
mycompany/api-gateway   Dockerfile:1:FROM golang:1.22
mycompany/auth-service  Dockerfile:1:FROM golang:1.22
✓ scan complete: 24/24 repo(s) searched, 18 match(es) in 9 repo(s)

# 4. DRY-RUN by default — opens nothing
$ goldfinger apply --branch bump-go --commit-message "Bump Go" \
    --pr-title "Bump Go" --sign local \
    -- sed -i 's|golang:1.22|golang:1.24|g' Dockerfile
▶ Applying to 24 repo(s) [dry-run — no push, no PRs] onto base each repo's default branch
dry-run: 24 repos — 9 would change, 15 no-change, 0 errors

# 5. for real — add --dry-run=false --confirm to open the 9 PRs
$ goldfinger apply … --dry-run=false --confirm -- sed -i '…' Dockerfile
▶ Applying to 24 repo(s) [LIVE — opening PRs] onto base each repo's default branch
✓ apply complete
```

<sub>Illustrative session — repo names and counts are examples; the line formats
(`▶`/`✓` banners on stderr, data on stdout) are exactly what goldfinger prints. A
recorded GIF can replace this block later.</sub>

> The repos you mirror, scan, and change are **provably the same selection** —
> frozen in one lockfile, so no filter can drift between phases.

## Why

An agent doing fleet work ("which repos still pin `golang:1.22`? patch this CVE
everywhere") that reaches for the GitHub API hits two walls: **rate limits** (5,000
REST req/hr, plus a stricter ~80 content-writes/min secondary limit) and
**latency** (every read is a paginated round-trip). goldfinger spends the API
budget only where it must:

1. **Resolve once, cheaply** — one read-only API pass turns `--org`/`--topic` into
   a concrete repo set, frozen in the lockfile.
2. **Read by cloning, not by API** — `mirror` + `scan` grep local clones; `git`
   isn't governed by REST limits, so the high-volume "what do I change?" work is
   free and fast.
3. **Write under the limit** — `apply` batches PR creation with pauses to stay
   under the secondary limit.

## Install

```sh
# Homebrew (also pulls in ghorg + multi-gitter automatically):
brew install redscaresu/tap/goldfinger

# or the one-line installer (grabs the right prebuilt binary, verifies its checksum):
curl -sSfL https://raw.githubusercontent.com/redscaresu/goldfinger/main/install.sh | sh
```

Releases carry [SLSA Level 3](https://slsa.dev/spec/v1.0/levels#build-l3)
provenance and per-asset SHA-256 sidecars, and the build is reproducible
(`make repro VERSION=<tag>` rebuilds the tag and prints a bit-for-bit-matching
hash). Prebuilt binaries, `go install`, and source-verification steps are all in
[`goldfinger guide`](#docs).

**Auth:** if you use the GitHub CLI there's nothing to set up — goldfinger picks up
your `gh auth login` session automatically. In CI (no interactive login), set
`GOLD_FINGER_PAT` to a PAT with Contents + Pull requests read/write. goldfinger
maps the one token to the env vars ghorg and multi-gitter each expect. You also
need a **git identity** (`git config user.name`/`user.email`) — multi-gitter
authors the `apply` commit from it.

## Commands

| Command | What it does |
|---|---|
| `select` | resolve repos by org/user + topic and freeze the lockfile |
| `mirror` | clone the frozen selection locally via ghorg (into `~/goldfinger`) |
| `scan <pattern>` | read-only regex search across the local mirror — no API, no rate limit |
| `apply … -- <cmd>` | run a change across the selection and open PRs (via multi-gitter) |
| `check` | diff the frozen lockfile against live discovery (drift detection) |
| `doctor` | preflight: token source, principal, child tools on PATH, signing |
| `selections` / `workspaces` | manage named selections and mirror snapshots |
| `guide` / `schema` | the input catalogue and the JSON-Schema output contract |

Every read command takes `--json` (machine data on stdout, human banners on
stderr) and `--quiet` for compact, token-cheap output. Exit codes are a stable
contract: `0` success, `1` a domain outcome (drift / failed check), `2` error.

For a one-off campaign, `mirror --purpose <name>` clones into a fresh, timestamped
`~/goldfinger/<name>-<stamp>` snapshot instead of the shared workspace, so parallel
efforts don't collide; `goldfinger workspaces` lists and prunes them.

## Safety model

- **`apply` is dry-run by default.** A real run additionally needs
  `--dry-run=false --confirm`, and an agent may only do it with explicit human
  authorization — otherwise the real run is the human's to execute.
- **`--sign` is required on every `apply`** — `local` (your GPG key via git),
  `github` (GitHub's web-flow key), or `none` (unsigned). There is no default;
  goldfinger states which mode it used.
- **Provable-same-set.** `mirror`, `scan`, and `apply` all read the one lockfile
  and never re-discover, so the repos you explore and the repos you change are the
  same list, in one artifact you inspect before anything runs.
- goldfinger **never runs `git` itself and never writes to GitHub directly** —
  discovery is read-only REST, mirroring is ghorg, PRs are multi-gitter.

## For AI agents

goldfinger is mostly operated by AI agents. `goldfinger guide` prints a compact
operator playbook that travels with the binary; the lockfile is JSON and every
error names the next action, so an agent can self-orient without this README.

`goldfinger mcp` serves goldfinger's **read-and-plan** surface over the
[Model Context Protocol](https://modelcontextprotocol.io) on stdio — the CLI's
machine surface as typed tools. Register it with Claude Code:

```sh
claude mcp add goldfinger goldfinger mcp
```

`apply` is deliberately **not** an MCP tool: opening PRs stays a human action.
`apply_plan` instead returns the exact, digest-bound `goldfinger apply` command
for a human to review and run.

Tell your own agents to reach for it — drop this into your repo's `AGENTS.md`:

```md
For any change spanning many repos (bump a dependency, patch a CI config, rotate a
setting), use the `goldfinger` CLI rather than hand-rolling clone/PR loops. Run
`goldfinger guide` for the playbook. A real `goldfinger apply --dry-run=false
--confirm` opens PRs and needs explicit human authorization — always dry-run first.
```

## Docs

- **`goldfinger guide`** — the full operator playbook (every command, flag, and
  auth/install detail), printed from the binary. `guide --json` is the
  machine-readable input catalogue.
- **`goldfinger schema`** — the JSON Schema (draft 2020-12) for the lockfile and
  every payload, so a consumer can validate goldfinger's output.
- **`AGENTS.md`** / **`CLAUDE.md`** — contributor-agent rules for changing
  goldfinger's own code.

## Development

```sh
make check   # go build + vet + race tests + lint (mirrors CI)
make e2e     # full-pipeline test against a sandbox repo (needs GOLD_FINGER_PAT + gh)
make hooks   # install the gitleaks pre-commit hook
```

CI runs the unit tests, gitleaks, govulncheck, and an end-to-end job that opens and
tears down a real PR on a sandbox repo.
