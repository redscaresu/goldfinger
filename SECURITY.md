# Security Policy

## Reporting a vulnerability

Please **do not** open a public GitHub issue for security vulnerabilities.
Instead, report privately via one of:

- GitHub's [private vulnerability reporting](https://github.com/redscaresu/goldfinger/security/advisories/new) (preferred — keeps the entire thread off public timelines).
- Email: `ukashouri@gmail.com` with subject prefix `[security] goldfinger:`.

Please include:

- A description of the issue and its impact.
- Steps to reproduce (or a proof-of-concept).
- Affected commit / version / branch.

## Handling of credentials

goldfinger resolves a single GitHub token and uses it for read-only API
discovery. It takes the token from the `GOLD_FINGER_PAT` environment variable if
set; otherwise it falls back to the local GitHub CLI session by shelling out to
`gh auth token` (so an interactive user needs no separate PAT). goldfinger never
writes to GitHub or runs `git` itself: mirroring and PR-fanout are delegated to
`ghorg` and `multi-gitter`.

The PAT is handed to those child tools only through the environment variables
they each expect — `GHORG_GITHUB_TOKEN` for ghorg, `GITHUB_TOKEN` for
multi-gitter — and **never on the command line** (a regression test asserts no
token appears in argv). The source `GOLD_FINGER_PAT` variable is stripped from
the child environment, so the raw PAT under that name never reaches a delegate
or a user-supplied `apply` script.

goldfinger streams the child tools' output straight through; it does not add a
redaction layer of its own, so it relies on ghorg and multi-gitter not printing
the token. If you find a path where a token can leak into logs, argv, output, or
committed files, treat it as a security issue and report it privately as above.

Some commands are entirely offline and touch no credential at all: `goldfinger
guide` (the operator playbook / CLI catalogue) and `goldfinger schema` (JSON
Schema for the lockfile and every machine-readable payload) resolve no token,
open no network connection, and run no child tool — they emit only static,
self-describing metadata. `goldfinger schema`'s output is derived solely from
goldfinger's own type definitions and never includes any selection data, token,
or environment value.

## Auditing the source

You do not have to take the claims above on trust. goldfinger keeps its
security-critical surface small and centralised on purpose, so each guarantee
can be verified by reading a named function and its regression test rather than
the whole codebase. This is the audit map.

### Threat model

What goldfinger is built to prevent:

- **Token leakage** — the operator's PAT reaching a process listing (argv), a
  child tool that doesn't need it, logs, or a committed file.
- **Unexpected mutation** — goldfinger changing GitHub or a local repo on its
  own. It performs **no** GitHub writes and runs **no** `git`; every mutation is
  delegated to ghorg (clone) or multi-gitter (commit/push/PR).
- **Silent scope drift** — the host environment changing *which* repos get
  mirrored or changed. The lockfile is authoritative; ambient config is scrubbed.
- **Command injection** — a user-supplied `apply` script breaking out of the way
  goldfinger hands it to multi-gitter.
- **An accidental live run** — `apply` opening PRs without an explicit,
  deliberate go-ahead.

### Audit map

Each row is a claim and the exact place to confirm it.

| Claim | Read | Confirmed by |
|---|---|---|
| The PAT is passed to child tools via the environment, never argv | `apply.overrideEnv` / `mirror.overrideEnv` set the token in the child env; the argv is built separately in each package's `buildArgs` and never receives it | `apply/apply_test.go` (token absent from joined argv, present in env); `mirror/mirror_test.go` (`assert.NotContains(... "secret-token")`) |
| The raw source PAT (`GOLD_FINGER_PAT`) never reaches either child tool's environment | `overrideEnv(..., models.TokenEnvVar)` strips the source var from the child env in both `apply` and `mirror`; only the mapped `GITHUB_TOKEN` / `GHORG_GITHUB_TOKEN` is added | `TestApplyStripsSourcePATFromChildEnv`, `TestMirrorStripsSourcePATFromChildEnv` (both assert on the child *environment*; neither executes the delegate) |
| The host can't silently narrow the mirror set or relocate clones | `mirror.ambientGhorgEnv` + `mirror.layoutGhorgEnv` are scrubbed from ghorg's env, and the layout knobs are pinned in argv | the scrub in `mirror.Mirror`; `mirror/mirror_test.go` |
| goldfinger only ever execs its two delegates plus read-only helpers — never a shell | the entire exec surface is three `exec.CommandContext` call sites: `cmd/exec.go` (the delegate `Runner`, explicit argv — no `sh -c`), `cmd/token.go` (`gh auth token`, literal args), `cmd/doctor.go` (`<tool> version`) | `grep -rn 'exec\.Command' --include='*.go'` returns exactly those three |
| A user-supplied `apply` command can't break out of quoting | every token is passed through `apply.shellQuote` (single-quoting, with any embedded `'` escaped) *before* it is written into the `#!/bin/sh` script line, so a token can't break out into an extra word or a command substitution; the script file itself is `0700` | `apply.writeScript` / `apply.shellQuote` |
| A real `apply` can't happen by accident, and never falls through to unsigned commits silently | `apply.Apply` refuses `DryRun=false` without `Confirm`, and refuses an empty/unrecognised `--sign` mode, at the execution boundary — not just the CLI layer. Unsigned commits remain *possible*, but only by deliberately passing `--sign none`; there is no default, so no run drifts into unsigned by omission | `TestApplyRefusesUnconfirmedLiveRun`, `TestApplyRequiresValidSignMode` |
| goldfinger writes nothing to GitHub and runs no `git` | there is no `exec.Command("git", ...)` and no go-github *write* call in the tree; discovery is read-only REST, all mutation is delegated to ghorg / multi-gitter | `grep -rn 'exec\.Command' --include='*.go'` shows no `git` exec — only the three delegate/helper sites above; the go-github calls in `client`/`discovery` are all reads (`Users.Get`, repository listing, `Repositories.GetBranch`) |

The design rules that keep this surface small and honest — flat packages, the
single exec seam, tokens via env not argv, the authoritative lockfile — are
documented for contributors in `AGENTS.md` under **Hard rules**, and CI enforces
them (race tests, `go vet`, `govulncheck`, and a secret scan).
