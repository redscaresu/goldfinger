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
