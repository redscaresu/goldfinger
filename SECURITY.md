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
token appears in argv).

Every variable that could carry a *GitHub* credential — or fetch one — is
stripped from the child environment first, and only then is the single variable
that delegate is meant to see added back. The canonical list is
`models.CredentialEnvVars`: `GOLD_FINGER_PAT`, `GITHUB_TOKEN`, `GH_TOKEN`,
`GHORG_GITHUB_TOKEN`, ghorg's GitHub App variables (`GHORG_GITHUB_APP_ID`,
`GHORG_GITHUB_APP_INSTALLATION_ID`, `GHORG_GITHUB_APP_PEM_PATH`,
`GHORG_GITHUB_TOKEN_FROM_GITHUB_APP`) and `GHORG_TOKEN_CMD`, which ghorg would
run as `sh -c` and use the output of as its token. So neither the raw PAT under
its source name nor any other *GitHub* credential the host happens to export
reaches a delegate or a user-supplied `apply` script.

The scope of that list is GitHub, deliberately, and the boundary is worth being
plain about: an `apply` script is the operator's own program and inherits the
rest of the environment goldfinger was run with. Unrelated secrets — cloud
credentials, other forges' tokens — are not filtered, because goldfinger is not a
sandbox and treating it as one would be the more dangerous mistake. What is
guaranteed is narrower and checkable: no ambient credential can change which
GitHub account a child acts as.

That matters beyond leakage — it is what makes the identity goldfinger *reports*
and the identity a child *acts as* the same thing:

- multi-gitter runs the `apply` script as a grandchild of goldfinger, and `gh`
  resolves `GH_TOKEN` ahead of `GITHUB_TOKEN`, so an unscrubbed ambient
  `GH_TOKEN` would let a script that shells out to `gh` open PRs as a different
  account than the one goldfinger resolved and announced.
- ghorg supports GitHub App authentication as an alternative to a token, so an
  ambient `GHORG_GITHUB_APP_*` set would let it clone as an App installation
  while goldfinger announces a PAT principal. `GHORG_GITHUB_APP_PEM_PATH` also
  points a child at a private key file no delegate goldfinger invokes needs.
- Which credential exists is only half of who a child acts as; the other half is
  where it is sent, and ghorg takes that from the environment too.
  `GHORG_SCM_BASE_URL` sets the API host — and those requests carry the mapped
  token, so one ambient variable makes a mirror run hand the operator's PAT to
  someone else's server. `GHORG_CLONE_PROTOCOL=ssh` swaps the token-bearing HTTPS
  clone URL for `git@github.com`, so the clone authenticates with whatever key
  the operator's ssh-agent holds and the announced identity is never used at all.
  `mirror` scrubs both (with `GHORG_SSH_HOSTNAME` and `GHORG_SCM_TYPE`) and
  additionally pins `--base-url=`, `--protocol=https` and `--scm=github` in argv.
  multi-gitter's equivalents — `base-url`, `ssh-auth`, `platform` — are occupied
  in the config `apply` writes.

Both hold by construction, not by the operator remembering to unset things.

Scrubbing the environment is necessary but **not sufficient**, and both delegates
fail closed only because of a second mechanism. The detail is counter-intuitive
enough to be worth stating outright: in each tool, a config file on the host beats
an *absent* environment variable.

- **ghorg** resolves each `GHORG_*` knob through its config file and then sets the
  result into its own environment, so a variable goldfinger strips is *re-created*
  from `~/.config/ghorg/conf.yaml` (or a stray `./ghorg.yaml` in the working
  directory) — including `GHORG_GITHUB_APP_PEM_PATH`, which switches ghorg to
  GitHub App auth after it has already built a client from our PAT. A ghorg CLI
  flag outranks the file, so `mirror` pins `--config` at an empty per-run YAML
  file, shutting off every key at once.
- **multi-gitter** does not have a config file goldfinger can switch off: an
  explicit `--config` is layered *above* the static `~/.multi-gitter/config`, not
  in place of it, and each file fills in any flag that is not already set. A
  static `token:` is therefore preferred over the `GITHUB_TOKEN` goldfinger sets,
  a static `org:`/`topic:`/`skip-repo:` changes the target set even though the
  lockfile is passed as `--repo` flags, and a static `skip-pr:`/`api-push:`
  changes what the run does and how its commits are signed. So the file `apply`
  writes is not empty: it *occupies* every key that decides who the run acts as,
  which repos it touches, or what it does to them — plus every knob goldfinger
  models as a flag of its own and prints in the dry-run digest, since it passes
  most of those only when non-empty — each with that key's neutral value, which is
  what stops the static file supplying one. Two are worth
  naming. `token: ""` blocks a static token while leaving multi-gitter to fall
  through to the environment, where goldfinger's own credential is. `skip-pr:
  false` blocks the worst outcome in the tool: multi-gitter checks out the
  feature branch only when it is opening a PR, so a single static `skip-pr: true`
  would push `HEAD` — still the *base* branch — turning a reviewed PR fanout into
  a direct push onto every selected repo's default branch. Four keys that meet
  the same rule are deliberately left out, and the reason is worth stating
  because it bounds the guarantee: `labels`, `reviewers`, `team-reviewers` and
  `assignees` are read through a helper that distinguishes an *unset* flag from
  one set to an empty list, and multi-gitter's GitHub layer treats the latter as
  "make the PR match this list" — so claiming them would strip the reviewers
  CODEOWNERS requested and the labels a repo's automation added, on every run.
  The residual, stated exactly: on runs where the operator named none of the
  four, a static value still reaches multi-gitter, which reconciles the PR *to*
  that value — so it can put metadata goldfinger never reported onto the PR, and
  on a re-run that updates an existing PR it can equally strip reviewers or
  labels added since. What it cannot touch is identity, the repo set, the action,
  or the signing path.

Neither is defence in depth on top of the env scrub; for these routes each is the
only thing standing.

goldfinger streams the child tools' output straight through; it does not add a
redaction layer of its own, so it relies on ghorg and multi-gitter not printing
the token. That is a real dependency, not a formality: ghorg prints the PAT under
`GHORG_DEBUG` by three routes — the resolved value of every variable it sets,
viper's settings dump, and the token-bearing HTTPS clone URL inside the
`spew.Dump` of each git command. `GHORG_DEBUG` is therefore scrubbed with the
auth-route category rather than left to the host with the other verbosity knobs,
and the empty `--config` keeps a ghorg config file from setting it back. A
verbosity knob is only safe to leave alone once you have checked what the extra
verbosity contains.

The same reasoning has to be applied one level further down, because **`git` is a
grandchild of goldfinger, not a child**. Both delegates put the resolved PAT
inside the HTTPS clone URL and then exec `git` without resetting the environment —
ghorg on every clone, multi-gitter under `--git-type=cmd`, which is what
`--sign=local` selects. So the token sits in `git`'s argv, and git has its own
variables that read that argv, rewrite it, or move the repository underneath it.

The scrub list is the result of classifying **every** git environment variable —
the union of `git(1)`'s ENVIRONMENT VARIABLES section and the `GIT_*` strings in
the shipped binary, 176 names — rather than of collecting the ones someone
happened to think of. A variable is scrubbed when it can disclose the token,
redirect where the token or the push goes, change which repository git acts on, or
change what content ends up in the clone, **and** removing it is monotonically
safer. That leaves four families:

- **Disclosure.** `GIT_TRACE` and the rest of the `GIT_TRACE*` family. Pointed at
  an absolute path, `GIT_TRACE` appends the full credential-bearing clone URL to
  that file, once per repo. `GIT_TRACE_REDACT=0` switches off the redaction git
  would otherwise apply, and `GIT_TRACE2_ENV_VARS` prints named variables' values.
- **Redirection.** `GIT_CONFIG_COUNT` with `GIT_CONFIG_KEY_n`/`GIT_CONFIG_VALUE_n`,
  and `GIT_CONFIG_PARAMETERS` (git's own `git -c` propagation channel, ranked at
  command-line precedence). An injected `url.<host>.insteadOf` rewrites the remote,
  sending the PAT to a host goldfinger never resolved and never announced. The
  prefix it matches on is not a guess: both delegates use a fixed username in the
  URL, so `https://oauth2:` is a literal. `GIT_SSL_NO_VERIFY` is here too — git
  tests its presence and never its value, so it can only ever stop git checking who
  it is talking to.
- **Retargeting.** `GIT_DIR`, `GIT_WORK_TREE`, `GIT_INDEX_FILE`,
  `GIT_OBJECT_DIRECTORY`, `GIT_ALTERNATE_OBJECT_DIRECTORIES`, `GIT_NAMESPACE`. Both
  delegates locate the repository purely by setting `cmd.Dir`, so an inherited one
  silently outranks the directory — verified reproductions include a commit from the
  operator's own repository pushed to a fleet remote, and a `git clean -f -d` that
  emptied a clone because a foreign index called every tracked file untracked.
- **Substitution.** `GIT_REPLACE_REF_BASE`, `GIT_SHALLOW_FILE`, `GIT_GRAFT_FILE`.
  These leave the remote alone and change what the clone appears to contain, so the
  run succeeds and the resulting PR looks ordinary while the change command reads
  and edits content or history the selected repository does not have.

All four are scrubbed from both delegates' environments
(`models.GitGrandchildEnvVars`). This is the one disclosure surface where the two
delegates are symmetric — multi-gitter's censoring log formatter, which is why its
log settings are otherwise left alone, cannot help here, because `git` writes these
files itself and multi-gitter never sees the bytes.

The exclusions are part of the classification, not gaps in it, and each is argued
in full at `models.gitGrandchildEnvVars`. The load-bearing ones: `GIT_CONFIG_GLOBAL`
and its siblings (removing the pointer does not remove an `insteadOf` rule, it falls
back to `~/.gitconfig` where the same rule can live — and `GIT_CONFIG_GLOBAL=/dev/null`
is the hardening idiom, so scrubbing inverts; it is also where an operator's
`commit.gpgsign` may live, so scrubbing would silently unsign a run that announced
`--sign=local`); `GIT_AUTHOR_*`/`GIT_COMMITTER_*` (goldfinger resolves and *announces*
these — `doctor` reports the identity they produce — and in a CI-shaped environment
they are the only identity git has, so removing them fails the commit rather than
correcting it); the hardening switches such as `GIT_TERMINAL_PROMPT` and
`GIT_NO_REPLACE_OBJECTS`, which can only make a run stricter than the default; and
git's exec knobs such as `GIT_SSH_COMMAND` (arbitrary execution is a broader claim
than goldfinger's two invariants, and an environment that can set them can set
`PATH`).

If you find another path where a token can leak into logs, argv, output, or
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
| No credential variable reaches a child tool except the one it needs — so the identity goldfinger announces is the identity every child acts as | `overrideEnv(..., models.CredentialEnvVars()...)` strips the whole credential set from the child env in both `apply` and `mirror`, then adds back only the mapped `GITHUB_TOKEN` / `GHORG_GITHUB_TOKEN`. `models.CredentialEnvVars` is the single canonical list (unexported backing slice, copy-returning accessor), and each delegate's own token var is declared beside it, so a delegate can never be handed a variable the scrub doesn't cover. The env scrub is only half the story for BOTH delegates, because each reads a host config file that outranks an absent variable — ghorg's re-creates `GHORG_GITHUB_APP_*`, and multi-gitter's static `~/.multi-gitter/config` supplies a `token:` that beats `GITHUB_TOKEN`. The two `--config` rows below are what close those | `TestApplyScrubsEveryCredentialVarFromChildEnv`, `TestMirrorScrubsEveryCredentialVarFromChildEnv` (assert exactly one credential var survives, and that no ambient credential *value* survives under any name); `TestCredentialEnvVarsCoversEveryTokenVar`; `TestApplyStripsSourcePATFromChildEnv`, `TestMirrorStripsSourcePATFromChildEnv`. All assert on the child *environment*; none executes the delegate |
| The two security helpers `apply` and `mirror` each keep a private copy of cannot silently diverge | `overrideEnv` (credential scrubbing) and `writeTempFile` (neutralising ambient host config) are duplicated by design — extracting them would export package-private API — so a test in *each* package compares the two copies' parsed source: signature, body, and every comment *inside* the declaration (an inline rationale or `//nolint` present in only one copy fails too). Comments outside it — the doc comment, which legitimately names each copy's own delegate — are exempt; an asymmetric declaration-level `//nolint` is caught by `make lint` instead, since the unsuppressed copy still gets flagged. The test file itself is duplicated, so a second test pins the two copies of it byte-identical | `TestDuplicatedDeclsHaveNotDrifted` and `TestDriftTestCopiesAreIdentical` in both `apply` and `mirror` |
| The host can't silently change which repos the mirror holds, where they land, what is inside them, where the token is spent, or which identity ghorg uses | `mirror.ambientGhorgEnv` + `mirror.layoutGhorgEnv` + `mirror.contentGhorgEnv` + `mirror.authRouteGhorgEnv` are scrubbed from ghorg's env — four categories chosen by a stated rule, so the rule can be reviewed rather than the list; the layout knobs (`--output-dir`, `--preserve-scm-hostname=false`) and the route (`--scm=github`, `--base-url=`, `--protocol=https`) are pinned in argv, where a ghorg flag outranks both env and config; `--config` is pinned at an empty per-run YAML file; and `--ghorgonly-path` is pinned at a path that deliberately does NOT exist — ghorgonly is an allowlist, so "off" is an absent file and an empty one would clone nothing | the scrub in `mirror.Mirror`; `TestMirrorNeutralisesAmbientConfig`, `TestMirrorPinsLayoutAgainstHostConfig`, `TestMirrorNeutralisesAmbientCloneContent`, `TestMirrorPinsAuthRouteAgainstHostConfig`, `TestMirrorPinsEmptyGhorgConfig` (exactly one `--config`, pointing at an existing, empty, parseable file, removed afterwards), `TestMirrorDisarmsGhorgonly` |
| The host's multi-gitter config can't change who apply acts as, which repos it touches, or what it does to them | `apply.multiGitterNeutralConfig` occupies every identity key (`token`, `username`, `base-url`, `platform`, `ssh-auth`), target-set key (`org`, `user`, `topic`, `skip-repo`, `repo-include`/`-exclude`, the searches, `fork`/`fork-owner`, …) action/signing key (`skip-pr`, `push-only`, `pr-auto-merge`, `dry-run`, `conflict-strategy`, `api-push`, `git-type`, `author-name`/`-email`, …) and digest key — the ones goldfinger passes only when non-empty, so a host default would silently fill them in (`base-branch`, `pr-body`, `draft`, `max-reviewers`, `output`, …) — with that key's neutral value, so the static `~/.multi-gitter/config` — which is layered *under* an explicit `--config`, not replaced by it — can no longer supply any of them. `labels`/`reviewers`/`team-reviewers`/`assignees` are the documented exception, held OUT because multi-gitter reads a set-but-empty slice as "remove all" | `TestApplyPinsNeutralisingConfig` (every key in `occupiedConfigKeys` present, each with the reason dropping it would matter, and `token` empty so the env still supplies it) |
| goldfinger only ever execs its two delegates plus read-only helpers — never a shell | the entire exec surface is three `exec.CommandContext` call sites: `cmd/exec.go` (the delegate `Runner`, explicit argv — no `sh -c`), `cmd/token.go` (`gh auth token`, literal args), `cmd/doctor.go` (`<tool> version`) | `grep -rn 'exec\.Command' --include='*.go'` returns exactly those three |
| A user-supplied `apply` command can't break out of quoting | every token is passed through `apply.shellQuote` (single-quoting, with any embedded `'` escaped) *before* it is written into the `#!/bin/sh` script line, so a token can't break out into an extra word or a command substitution; the script file itself is `0700` | `apply.writeScript` / `apply.shellQuote` |
| A real `apply` can't happen by accident, and never falls through to unsigned commits silently | `apply.Apply` refuses `DryRun=false` without `Confirm`, and refuses an empty/unrecognised `--sign` mode, at the execution boundary — not just the CLI layer. Unsigned commits remain *possible*, but only by deliberately passing `--sign none`; there is no default, so no run drifts into unsigned by omission | `TestApplyRefusesUnconfirmedLiveRun`, `TestApplyRequiresValidSignMode` |
| goldfinger writes nothing to GitHub and runs no `git` | there is no `exec.Command("git", ...)` and no go-github *write* call in the tree; discovery is read-only REST, all mutation is delegated to ghorg / multi-gitter | `grep -rn 'exec\.Command' --include='*.go'` shows no `git` exec — only the three delegate/helper sites above; the go-github calls in `client`/`discovery` are all reads (`Users.Get`, repository listing, `Repositories.GetBranch`) |

The design rules that keep this surface small and honest — flat packages, the
single exec seam, tokens via env not argv, the authoritative lockfile — are
documented for contributors in `AGENTS.md` under **Hard rules**, and CI enforces
them (race tests, `go vet`, `govulncheck`, and a secret scan).
