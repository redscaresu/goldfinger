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

goldfinger reads a GitHub PAT from `GITHUB_TOKEN` and embeds it in clone URLs
for push authentication. The token is redacted from all command output and
error messages before they leave the tool; a regression test enforces this. If
you find a path where a token can leak into logs, output, or committed files,
treat it as a security issue and report it privately as above.
