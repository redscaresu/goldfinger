# goldfinger

[![CI](https://github.com/redscaresu/goldfinger/actions/workflows/ci.yml/badge.svg)](https://github.com/redscaresu/goldfinger/actions/workflows/ci.yml)
[![coverage](https://raw.githubusercontent.com/redscaresu/goldfinger/badges/coverage.svg)](https://github.com/redscaresu/goldfinger/actions/workflows/ci.yml)
[![zizmor](https://github.com/redscaresu/goldfinger/actions/workflows/zizmor.yml/badge.svg)](https://github.com/redscaresu/goldfinger/actions/workflows/zizmor.yml)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/redscaresu/goldfinger/badge)](https://scorecard.dev/viewer/?uri=github.com/redscaresu/goldfinger)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/13997/badge)](https://www.bestpractices.dev/projects/13997)

An orchestration layer for fleet-wide GitHub work. goldfinger resolves a set of
repos once — by org/user and topic — freezes it as a reviewable **selection**,
then drives two best-in-class tools against that exact set:

- **[ghorg](https://github.com/gabrie30/ghorg)** to mirror the selection into a
  persistent local workspace (clone/pull hundreds of repos, kept fresh).
- **[multi-gitter](https://github.com/lindell/multi-gitter)** to apply a change
  across the selection and open PRs.

The value goldfinger adds is not mirroring or PR-fanout — those tools already do
each well. It's that fleet work stays **cheap and rate-limit-safe** (resolve the
set with one API call, then read via local clones instead of the API) and that
**the repos you mirror and the repos you change are provably the same selection**,
frozen in one artifact you inspect before anything runs. (The mirror is your local
read/test copy; multi-gitter re-clones the same repos to apply the change — it's
the *set* that's shared, driven by the one lockfile, not a clone.) It's **built to
be driven by AI agents as much as by people** — see [For AI agents](#for-ai-agents).

## It's a wrapper over ghorg and multi-gitter

goldfinger clones nothing and opens no PRs itself — it shells out to two mature
CLIs and coordinates them around one shared selection:

| You run | goldfinger shells out to | which does the work |
|---|---|---|
| `goldfinger mirror` | `ghorg clone <owner> --target-repos-path=<names> --path=<ws>` | clone/pull the repos locally |
| `goldfinger apply … -- <cmd>` | `multi-gitter run <script> --repo a/b --repo a/c …` | run the change + open PRs |

Everything goldfinger *itself* does is the glue around those two calls:

- **Feed both tools the identical set** — the lockfile is handed to ghorg via a
  `--target-repos-path` names file and to multi-gitter via repeated `--repo`
  flags. Neither tool re-discovers, so the two phases can't drift apart.
- **Map one token, check the tools** — goldfinger resolves a single GitHub token
  and maps it to the env vars each tool expects (`GHORG_GITHUB_TOKEN`,
  `GITHUB_TOKEN`), checks both are installed, and frames their output. (Auth
  setup is in [Install](#install) — it reuses your `gh` login by default.)

## Why this exists

goldfinger is built primarily for agents. An agent doing fleet work — "which of
our repos still pin `golang:1.22`?", "patch this CVE everywhere" — reaches for the
GitHub API to list repos, read files, and search code, and immediately hits two
walls: **rate limits** (5,000 REST requests/hour, plus a stricter secondary limit
of ~80 content-writing requests/minute) and **latency** (every read is a paginated
round-trip). At fleet scale that's slow and quota-hungry.

goldfinger spends the API budget only where it has to:

1. **Resolve once, cheaply.** A single read-only API pass turns `--org` / `--topic`
   into a concrete repo set, frozen in the lockfile. The topic / `--all-repos`
   filter means you only pull the repos you actually need.
2. **Read by cloning, not by API.** `mirror` hands that set to `ghorg`, which
   `git clone`s them locally. `git` isn't governed by the REST rate limits, and
   grepping a local checkout is far faster than paginating the contents/search
   API — so the high-volume "figure out what to change" work is cheap and fast.
3. **Write under the limit.** `apply` batches PR creation with pauses so the
   content-writing phase stays under the secondary rate limit instead of tripping
   it partway through a fleet.

A useful side effect of freezing the set up front: the repos you *explore* and the
repos you *change* are provably the same list — no filter drift between phases.

## The model

```
                 ┌──────────────────────────────┐
   select  ───▶  │  goldfinger.selection  (lock) │  owner/name list + provenance
                 └──────────────────────────────┘
                        │                    │
             mirror ────┘                    └──── apply
          (ghorg clone/pull            (multi-gitter run:
           the exact set into           script + PR across
           a local workspace)           the exact set)
```

## Quickstart

```sh
# Auth: uses your `gh auth login` session automatically; set GOLD_FINGER_PAT
# instead in CI. Details under Install, below.

# 1. freeze the target set -> ./goldfinger.selection
goldfinger select --org mycompany --topic platform

# 2. clone them locally to grep and inspect (optional; not needed to open PRs)
goldfinger mirror
# for a one-off campaign, get an ephemeral timestamped snapshot instead:
#   goldfinger mirror --purpose bump-go   # -> ~/goldfinger/bump-go-<timestamp>/<owner>/

# 3. dry-run the change — prints a per-repo status digest, opens nothing (--sign is always required)
goldfinger apply --branch bump-go --commit-message "Bump Go" --pr-title "Bump Go" \
  --sign local -- sed -i 's|golang:1.22|golang:1.24|g' Dockerfile

# 4. for real — opens the PRs (requires both flags)
goldfinger apply --branch bump-go --commit-message "Bump Go" --pr-title "Bump Go" \
  --sign local --dry-run=false --confirm -- sed -i 's|golang:1.22|golang:1.24|g' Dockerfile
```

`goldfinger guide` prints this playbook from the binary itself. `goldfinger
guide --json` instead emits a versioned, machine-consumable catalogue of the CLI
surface (every command, its flags, which flags are required, a flag's enum
values, and a canonical example per command) on stdout — so an agent can discover
what goldfinger can do by parsing structure rather than prose. Its output-side
companion is `goldfinger schema`, which prints the JSON Schema for the lockfile
and every machine-readable payload, so a consumer can validate what goldfinger
emits. See [machine-readable output](#machine-readable-output---json).

### `goldfinger select`

Resolves the repo set via the GitHub API and writes the selection lockfile.

```sh
goldfinger select --org mycompany --topic platform --topic payments
# or every non-archived repo:
goldfinger select --org mycompany --all-repos
```

- `--org <owner>` — a GitHub org **or** user (required).
- `--all-repos` or `--topic <t>` (repeatable, any-match) — exactly one is
  required. Topic filtering is goldfinger's, applied here and frozen into the
  lockfile; ghorg's and multi-gitter's own topic flags are bypassed.
- `--branch-presence <name>` (repeatable) — for each named branch, record
  (read-only) whether it exists on every selected repo, and freeze that into the
  lockfile so a later `mirror` can report which repos actually have it. Run it
  for the branch you intend to `mirror --branch`, e.g. `select … --branch-presence
  dev`. A name equal to a repo's own default branch is present by definition
  (no API call); duplicates are deduped. These facts are **recorded at selection
  time and can drift** — re-`select` to refresh them.
- `--allow-empty` — by default a filter that matches **zero repos** is an error
  (exit `2`) and no lockfile is written, because a zero-repo result almost always
  means a wrong token identity, a wrong owner, or a topic that matches nothing —
  not an intended empty fleet. The error names the authenticated identity and the
  inputs so you can tell those apart. Pass `--allow-empty` for the rare case where
  an empty selection is genuinely what you want.
- **Auth transparency:** unless `--quiet` is set, every run prints, on
  **stderr**, which credential it used (`auth: using …`) and the GitHub login it
  resolved to (`auth: authenticated as …`) — so a wrong-token result is
  diagnosable at a glance. If the token came from
  your `gh` session and a stray `GITHUB_TOKEN`/`GH_TOKEN` is set in the
  environment, it also warns that `gh` may be using that ambient token instead of
  your stored login. The token value itself is never printed.
- `--list` — echo every selected repo's full name on **stdout**, one per line.
  By default stdout stays **terse**: the repo count goes to stderr (`N repo(s)
  written to … (digest <hash>)`) and the full list lives in the lockfile, so a
  large selection doesn't dump one stdout line per repo onto a driving agent.
  Pass `--list` when you want the names back on stdout, or `--json` for the full
  wrapper. (Behaviour change: earlier versions printed the repo list on stdout by
  default; that echo is now `--list`.)
- **Selection digest.** Every `select` reports a short repo-set fingerprint — a
  12-hex-char (48-bit) hash over the sorted repo full-names — on the stderr
  `done` line (`… (digest <hash>)`) and, under `--json`, as a top-level `digest`
  field. It covers the repo **set** only (order-independent; branch-presence and
  provenance don't change it). It is a **change detector**, not a cryptographic
  commitment: a *differing* digest means the repo set definitely changed, and a
  *matching* one is strong (but, being truncated, not absolute) evidence the set
  is unchanged. A driving agent can compare digests to spot "same N repos"
  cheaply without reading the whole lockfile back. `selections` shows it as a
  `DIGEST` column (and a `digest` field under `--json`).
- Writes `goldfinger.selection`; the lockfile is plain JSON — inspect or diff it
  before mirroring/applying:

```json
{
  "version": 2,
  "owner": "mycompany",
  "ownerType": "Organization",
  "filter": { "topics": ["platform"] },
  "resolvedAt": "2026-08-01T15:17:53Z",
  "tool": "goldfinger v0.3.0",
  "branchesChecked": ["dev"],
  "repos": [
    { "owner": "mycompany", "name": "billing", "cloneURL": "https://github.com/mycompany/billing.git", "defaultBranch": "main", "topics": ["platform"], "branchPresence": { "dev": true } }
  ]
}
```

`version` is `2`; a `version: 1` lockfile (no `branchesChecked` /
`branchPresence`) still reads, with every branch treated as **unknown** rather
than guessed. `branchesChecked` and each repo's `branchPresence` appear only
when you passed `--branch-presence`.

### `goldfinger mirror`

Reads the lockfile and mirrors exactly that set locally via ghorg.

```sh
goldfinger mirror
```

This step is **optional**. It exists so you can read and scan the fleet locally
(grep, run scanners, develop your change script). It is **not** required to open
PRs — [`apply`](#goldfinger-apply) clones each repo on its own, so the two never
share a directory.

Shells out to `ghorg clone <owner> --target-repos-path=<names> --path=<workspace>`
so ghorg clones/pulls only the selected repos into goldfinger's workspace. Re-run
any time to refresh; ghorg pulls existing clones instead of re-cloning.

By default a re-sync also `git clean`s each clone, so the workspace stays a
pristine reflection of upstream (local edits are discarded). Pass `--no-clean`
to preserve local changes across re-syncs — i.e. treat the mirror as an editable
workspace rather than a read-only reflection. Other passthroughs: `--concurrency`,
`--clone-depth` (shallow), `--dry-run`.

Pass `--branch <name>` to check out a specific branch in every clone instead of
each repo's default. It is **one name applied to all repos**: ghorg leaves a repo
on its default branch where that branch is absent, so it's a best-effort "prefer
`dev` where it exists", not a per-repo guarantee (the lockfile records each
repo's own default branch for that).

`--branch` and `--clone-depth` are **incompatible** — goldfinger refuses the
combination. A shallow clone (`--clone-depth 1`) only fetches each repo's
**default** branch, so `mirror --branch dev --clone-depth 1` would silently leave
every repo where `dev` isn't the default on its default branch: a false-coverage
trap. Omit `--clone-depth` (full depth) when mirroring a non-default `--branch`;
shallow stays fine for a plain default-branch scan.

The resolved workspace path is printed as a bare absolute path to **stdout**
(banners and ghorg's own output go to **stderr**), so a script can capture it —
`ws=$(goldfinger mirror --purpose keyv-cve 2>mirror.log)` — instead of globbing
for the millisecond-stamped dir. Because the path prints before ghorg runs, check
the exit code (not stdout) to confirm the clone succeeded.

When ghorg finishes, goldfinger prints its own **reconciliation line** to stderr:

```
✓ reconciliation: in selection: 59 | on disk: 59 | branch present: 15 | fell back: 44
```

Read this instead of ghorg's `N new clones`, which counts only *newly* cloned
repos — a re-mirror of an unchanged fleet reports `0 new clones` even though all
59 are present. `in selection` is the lockfile count; `on disk` is a read-only
count (a directory stat per repo, no `git`) of how many actually landed under
`<workspace>/<owner>`. If `on disk` is short, goldfinger warns (⚠) that the
mirror under-covered the selection so you can re-run rather than trust a green
finish. With `--branch`, `branch present` / `fell back` (and `unknown`, when any)
recast ghorg's per-repo `Could not checkout <branch>` noise as the expected
fall-backs they are — the same facts as `--report-json`'s `branchStatus` below.

ghorg's live output still streams to stderr as before, but goldfinger also
**captures the full run to a `0600` temp log** and prints its path (`✓ ghorg
output captured at …`), so after the terse reconciliation summary you can drill
into the clone errors behind a shortfall without scrolling back. On a failed
mirror the same path is surfaced with a `⚠` — a partial clone is exactly when
those errors matter. (Under `--quiet` the output is discarded, as before: no
live stream, no log.)

To know *which* repos actually had the branch (rather than silently falling
back), record it at `select` time with `--branch-presence <name>` and ask
`mirror` for a report:

- `--report-json` prints a machine-readable JSON report to **stdout** after a
  successful mirror. It *replaces* the bare workspace-path line above — stdout
  carries one or the other, so the JSON stays parseable (the path is a field in
  the report).
- `--write-report` writes the same JSON to `<workspace>/goldfinger-mirror.json`
  (only on success — a failed clone leaves no report).

Every fact in it comes from the two sources goldfinger can read without running
`git` or re-running discovery: the lockfile (`workspace`, `owner`, `repoCount`,
the requested `branch`, and each repo's `branchStatus`) and a read-only
filesystem stat (the `reconciliation` block — the same coverage counts as the
stderr line above, so an agent parsing `--report-json` under `--quiet` gets them
too):

```json
{
  "workspace": "/Users/me/goldfinger",
  "owner": "mycompany",
  "repoCount": 2,
  "branch": "dev",
  "branchFactsNote": "branchStatus values come from branch presence recorded at selection time (via `select --branch-presence`) and can drift; \"unknown\" means the branch was not checked then — goldfinger does not guess it here.",
  "reconciliation": {
    "inSelection": 2,
    "onDisk": 2,
    "notOnDisk": 0,
    "branch": { "present": 1, "fellBack": 1, "unknown": 0 }
  },
  "repos": [
    { "repo": "mycompany/billing", "defaultBranch": "main", "branchStatus": "has-branch" },
    { "repo": "mycompany/web", "defaultBranch": "main", "branchStatus": "falls-back-to-default" }
  ]
}
```

In `reconciliation`, `inSelection` is the lockfile count, `onDisk` is how many
repos actually landed under `<workspace>/<owner>`, and `notOnDisk` is the
shortfall (repos that failed to land — a real coverage gap, distinct from a
branch fall-back). The nested `branch` object appears only when `--branch` was
requested (its `present`/`fellBack`/`unknown` tallies sum to `inSelection`); a
no-branch mirror carries no `branch` object rather than a block of ambiguous
zeros.

`branchStatus` is `has-branch` (the branch was present at select time, or is the
repo's own default), `falls-back-to-default` (absent at select time, so ghorg
stays on the default), or `unknown` (the branch was never checked at select time
— an old lockfile, or no `--branch-presence` for it; goldfinger does **not**
guess). With no `--branch`, every repo reports `default-branch`.

**For a one-off mass-PR campaign, use `--purpose` for an ephemeral, timestamped
workspace** — you supply the purpose, goldfinger stamps the time to the
millisecond so each run gets its own pristine dir:

```sh
goldfinger mirror --purpose keyv-cve --clone-depth 1
# clones into ~/goldfinger/keyv-cve-2026-08-04-132045.123/<owner>/
# ...scan / develop the change against that snapshot...
```

Add `--branch` and it is folded into the dir name too, as
`<purpose>-<branch>-<stamp>` (a branch's slashes become dashes):

```sh
goldfinger mirror --purpose keyv-cve --branch dev
# clones into ~/goldfinger/keyv-cve-dev-2026-08-04-132045.123/<owner>/
```

A `--purpose` mirror also drops a small sidecar manifest at the snapshot root
(`goldfinger-workspace.json`, recording `purpose`, `branch`, `stamp`, `owner`,
`createdAt` as separate fields) on a successful, non-dry-run mirror. It exists so
`goldfinger workspaces list | prune` (below) get reliable structured metadata:
the directory name alone can't be split back into its parts, because both the
purpose and a sanitised branch can contain `-`.

goldfinger **never deletes** the directory — it persists so you can review it.
Reclaim old snapshots with [`goldfinger workspaces prune`](#goldfinger-workspaces)
(preview-by-default, deletes only with `--confirm`), or just remove one yourself
(`rm -rf ~/goldfinger/keyv-cve-2026-08-04-132045.123`). A fresh per-campaign clone is
pristine by construction, so it sidesteps a trap
long-lived clones fall into: if upstream rebases or squashes its default branch,
a stale persistent clone can no longer fast-forward and `git pull` fails with
exit 128. `git clean` removes untracked *files*, not *commits*, so it cannot
recover a *diverged* clone — only a fresh clone (or a manual `git reset --hard`)
can. The durable artifact worth keeping is the **lockfile**, not the clones.

To keep the lockfile authoritative, goldfinger neutralises ambient ghorg config
that could change the set behind your back: it strips set-narrowing/pruning
`GHORG_*` environment variables (`GHORG_TOPICS`, `GHORG_MATCH_REGEX`,
`GHORG_SKIP_ARCHIVED`, `GHORG_PRUNE*`, …) from ghorg's environment and forces an
empty `--ghorgignore-path`, so a stray env var or `~/.config/ghorg/ghorgignore`
can't silently drop repos.

### `goldfinger apply`

Reads the lockfile and runs a change across exactly that set via multi-gitter.

```sh
goldfinger apply --branch bump-go-1.24 \
  --commit-message "Bump golang base image to 1.24" \
  --pr-title "Bump golang base image to 1.24" \
  --sign local \
  -- sed -i 's|golang:1.22|golang:1.24|g' Dockerfile
```

Shells out to `multi-gitter run`, passing one `--repo owner/name` per lockfile
entry plus the script and PR flags. multi-gitter clones each repo into its own
temporary directory, applies the change, pushes, and cleans up — it does **not**
use the `mirror` workspace, so `apply` works whether or not you ran `mirror`.
It branches from the **current HEAD of the base branch at apply time** — not the
SHA you mirrored in step 1 — so PRs are always against live base. If a repo's
code moves between your inspection and the apply, the change runs against the
newer code; run `--dry-run` first (it also clones fresh) rather than trusting the
mirror snapshot. Non-interactive multi-gitter dry-run does **not** emit a unified
diff; goldfinger prints the per-repo status it can honestly know
(`would-change`, `no-change`, or `error`) plus a full-output file path. (`check`
catches *selection* drift, not *content* drift inside a repo — dry-run is the
apply-time signal.)

- The command after `--` runs in each repo's checkout, **on your machine** — so
  it must be portable to your OS. `sed -i 's|…|…|g'` is a GNU-ism that fails on
  macOS (BSD `sed` needs `sed -i ''`). For anything non-trivial, or edits that
  differ per file, pass a script instead of an inline command — e.g.
  `-- python3 /abs/path/migrate.py` — which is portable and can carry per-file
  logic a single `sed` can't.
- **Base branch.** With `--base-branch` omitted, each PR targets that repo's
  **own default branch** — so a mixed `dev`/`main` selection routes correctly
  per repo with no extra flags. Pass `--base-branch <name>` to force one branch
  across the whole set (note: it's a single global value — if some repos default
  to `main` but you want `dev` there specifically, split the selection instead).
- Other PR options: `--pr-body` (or `--pr-body-file <path>` to load a long body
  from a file — the two are mutually exclusive), `--label` / `--reviewer`
  (repeatable), `--draft`.
- **Signing (`--sign`, required — no default).** Every run must state how commits
  are signed; there is deliberately no default, because commit provenance is not
  a safe thing to leave implicit for a fleet-wide, hard-to-reverse action. Three
  modes, each with a different trust model:
  - `--sign local` — runs the change through the real `git` binary
    (multi-gitter `--git-type=cmd`), so your `~/.gitconfig` `commit.gpgsign` /
    `user.signingkey` apply and each commit is signed with **your own GPG key**.
    It shows as "Verified" on GitHub only if that public key is uploaded to your
    account. Trade-offs: your `gpg-agent` must have the passphrase cached for the
    whole run — a cold or kicked agent (e.g. a headless/background session) can
    stall on per-commit `pinentry`; warm it first (`echo test | gpg --clearsign
    >/dev/null`). *(Verified against multi-gitter v0.63.1: `--git-type=cmd` runs
    `git commit` with no `-S` and no `--no-gpg-sign`, so your `commit.gpgsign`
    config is what signs the commit. This relies on goldfinger not passing
    `--author-name`/`--author-email`, which would strip the commit's environment
    and break signing.)*
  - `--sign github` — pushes commits through the GitHub API (multi-gitter
    `--api-push`), signed by **GitHub's own web-flow key** (always "Verified", no
    local key or `pinentry`). GitHub-only, slower, and **unsuited to large
    files** — and it interacts with the same secondary rate limits as PR creation
    (see below).
  - `--sign none` — **unsigned** commits (multi-gitter's default `go-git` path).
    An explicit, deliberate opt-out; the dry-run banner flags it loudly.
- **Safety:** `apply` defaults to `--dry-run` (prints a per-repo status digest
  and opens nothing). A real run requires **both** `--dry-run=false` **and**
  `--confirm` — the guard against an accidental fleet-wide PR blast. A real run
  should always follow a reviewed dry-run; when an agent runs it under explicit
  human authorization, prefer `--draft`.
- **`--plan-json` (machine-readable plan).** Emits, on stdout, a structured
  summary of **what goldfinger is about to invoke** — branch, PR title, sign mode,
  the base-branch source, and one entry per repo — so an agent can present the plan
  crisply. It is **invocation metadata, not the diff**: goldfinger delegates the
  clone/script/diff to multi-gitter, so the plan never fabricates per-repo
  `changed` flags or a diffstat. Two safety points: the
  script after `--` is emitted only as `command_program` (argv[0]) with
  `command_redacted: true` (your script is arbitrary and may carry secrets), and
  the PR body is reduced to `pr_body_present` (a boolean). `--plan-json`
  **supplements** the dry-run — it does not replace it: goldfinger still runs
  multi-gitter's `--dry-run` so you get both the plan (stdout) and the status
  digest plus full-output file path (stderr — under `--quiet` the plan keeps
  stdout and the digest is suppressed). `base_branch_recorded` is the value
  **recorded at selection time**;
  with no `--base-branch`, multi-gitter targets each repo's *live* default at run
  time, which can drift (same caveat the dry-run banner prints) — the guarantee
  between phases is set-identity, not commit-SHA-identity.
- **Config isolation:** goldfinger invokes multi-gitter with an empty `--config`
  so an ambient `multi-gitter` config file can't inject its own repo/org
  selection or filters — the lockfile's `--repo` set is the only source of truth
  for which repos get changed.
- **Rate limits.** Opening PRs across a large fleet can trip GitHub's *secondary*
  rate limits — separate from the 5,000/hr REST budget. GitHub allows **80
  content-generating requests per minute** and **500 per hour**, and a single PR
  is more than one such request: the PR itself, plus one each for `--label` and
  `--reviewer`. So a PR with labels + reviewers costs ~3–4, putting the real
  ceiling around ~150 PRs/hour (or ~500 with none).
  - `--batch-size N` opens PRs in chunks of `N` repos, and `--batch-pause D`
    sleeps `D` (e.g. `90s`) between chunks. This keeps you under the **80/min**
    limit — size a batch so `N × requests-per-PR` stays well below 80 and pause
    ≥ `60s`. Example: `--batch-size 15 --batch-pause 90s`.
  - Batching **cannot** beat the **500/hour** ceiling — no within-hour pacing
    can. For a fleet past that, the run will eventually hit the hourly limit and
    error; just **re-run the same `apply`** after the hour resets. multi-gitter's
    default `conflict-strategy: skip` means repos that already have a branch/PR
    are skipped, so a re-run only attempts the remainder — apply is naturally
    resumable, which is how you spread a big fleet across hours.

### `goldfinger doctor`

A read-only preflight for "can this machine actually run goldfinger?" — run it
once on a new box or in CI before the first real command:

```sh
goldfinger doctor
goldfinger doctor --json
```

It prints one line per check and **never** writes to GitHub, runs `git`, or
prints the token:

- **auth** — which token *source* and GitHub *principal* a run would use
  (`authenticated as <login> via GOLD_FINGER_PAT` / `via local gh session`). A
  fail if no token resolves or it doesn't verify.
- **auth-shadow** — warns if an ambient `GITHUB_TOKEN`/`GH_TOKEN` may be
  shadowing your `gh` login (the wrong-identity footgun — see
  [Requirements](#requirements)).
- **ghorg** / **multi-gitter** — each child tool's PATH location and version, or
  a fail with an install hint if it's missing.
- **git-identity** — `user.name`/`user.email` resolved by *parsing* the
  system + global + env git config directly (goldfinger never shells out to
  `git`). A warn if unset, because `apply` would then silently make no commit and
  open no PR. If an `include`/`includeIf` makes the config unresolvable and no
  identity was found in the parts read, it degrades to a warn rather than a false
  pass.
- **signing** — `commit.gpgsign` / `user.signingkey` readiness for
  `--sign local`. Advisory only (never a fail): `--sign github`/`--sign none`
  don't depend on local git config.

Exit status: `0` nothing failed, `1` a check failed (no token, a missing child
tool), `2` doctor itself could not run. Warns and info never fail the run, so
`doctor` is a safe CI gate.

### `goldfinger check`

A selection is frozen at `select` time, but repos get created, archived, and
renamed. Before a large mirror or apply, `check` tells you whether the lockfile
still matches reality:

```sh
goldfinger check                 # ./goldfinger.selection
goldfinger check --name platform # a named cohort
```

It re-runs discovery using the selection's **own recorded filter** and diffs the
result against the lockfile, reporting repos added (`+`), removed with a reason
(`-`, e.g. archived / no longer matches / deleted), whose default branch has
moved (`~`, which changes where an apply PR would land), or whose owner type has
flipped (`!`). It is read-only — it never rewrites the lockfile (re-run `select`
to refresh) and never re-runs discovery inside `mirror`/`apply`, so that
guarantee is preserved. Exit status makes it usable as a CI gate: `0` in sync,
`1` drift found, `2` error.

### Named selections

By default a selection lives in `./goldfinger.selection`. To keep several
standing cohorts, give each a **name** — stored in a registry
(`~/.config/goldfinger/selections/<name>.json`) and referred to by `--name` on
any command:

```sh
goldfinger select --name platform --org acme --topic platform   # define / refresh
goldfinger select --name payments --org acme --topic payments
goldfinger selections                                           # list them
goldfinger mirror --name platform                               # operate by name
goldfinger apply  --name payments -- sed -i '…' Dockerfile
```

`--name` and `--selection <path>` are mutually exclusive; re-running
`select --name X` refreshes that cohort in place.

### `goldfinger workspaces`

Every `mirror --purpose` leaves a timestamped snapshot dir under the workspace
root (default `~/goldfinger`), and goldfinger never deletes them on its own — so
they accumulate. `workspaces` is the safe, first-class way to see and reclaim
them. It is **filesystem-only**: it never touches GitHub and runs no git.

```sh
goldfinger workspaces list           # size + creation time per snapshot
goldfinger workspaces list --json    # {version, root, action, workspaces:[…]}

goldfinger workspaces prune                    # PREVIEW — shows what it would
                                               # remove, deletes nothing
goldfinger workspaces prune --confirm          # actually delete
goldfinger workspaces prune --older-than 168h  # only snapshots older than 7d
goldfinger workspaces prune --purpose keyv-cve # only that recorded purpose
```

`list` enumerates only the stamped snapshot dirs (those ending in `-<stamp>`);
the default `~/goldfinger/<owner>` mirror and any unrelated directory are
ignored. A snapshot's `purpose`/`branch`/`owner` come from its sidecar manifest
(`goldfinger-workspace.json`) when present; a legacy manifest-less snapshot is
still listed — `createdAt` is recovered from the dir-name stamp — but with no
structured purpose/branch, because the directory name is **not** reliably
splittable back into its parts.

`prune` mirrors `apply`'s safety posture: it **previews by default and deletes
only with `--confirm`** — it never removes a snapshot on its own initiative, and
there is no time-based auto-GC. Narrow the target with `--older-than <dur>`
and/or `--purpose <name>`; with no filter it targets every snapshot (still
confirm-gated). Both filters are deliberately conservative:

- `--purpose` matches **only** manifest-tagged snapshots whose recorded purpose
  is *exactly* that name — a manifest-less snapshot is never matched by purpose.
- `--older-than` skips any snapshot whose age can't be determined, so an
  ambiguous snapshot is kept, not deleted.

`prune` will only ever delete a stamp-suffixed directory that is a direct child
of the root, re-checked immediately before each removal as defence in depth.
`--older-than`/`--purpose`/`--confirm` apply to `prune` only; passing them to
`list` is an error.

> **Not yet: a `--single-branch` clone.** A single-branch, full-depth clone (one
> branch, full history) would cut mirror size without the shallow-clone trap, but
> [ghorg](https://github.com/gabrie30/ghorg) (as of v1.11.14) exposes no
> single-branch flag and goldfinger will not reimplement cloning — so this is
> **deferred pending an upstream ghorg option**. For now `--clone-depth 1`
> (shallow, default branch only) is the size lever; omit it for a full clone.

### Machine-readable output (`--json`)

Every read command emits structured JSON on request, following one contract:
**stdout is machine data, stderr is human banners/logs.** In `--json` mode the
JSON is the *only* thing on stdout — banners, progress, and the auth lines all go
to stderr — so an agent can parse stdout without stripping prose. The global
`--quiet` / `-q` flag silences that human stderr stream and, for non-JSON
commands, reduces stdout to the one machine result described below. Under
`--quiet` these JSON payloads are also emitted compact (single-line) rather than
indented — same shape, fewer tokens; see [Designed for
agents](#designed-for-agents-reducing-token-consumption).

| Command | Flag | Payload |
| ------- | ---- | ------- |
| `select` | `--json` | `{selectionPath, selection, digest}` — `selection` is the full lockfile object exactly as persisted (its own `version` field is the payload version); `digest` is the short repo-set fingerprint (see [`select`](#goldfinger-select)). |
| `doctor` | `--json` | `{version, checks:[{check, status, detail, fix?}]}` where `status` is `ok`/`info`/`warn`/`fail`. The token value is never included. See [`doctor`](#goldfinger-doctor). |
| `check` | `--json` | `{version, name?, inSync, added, removed:[{repo,reason}], defaultBranchMoved:[{repo,from,to}], ownerTypeFlipped:{from,to}\|null}`. Exit code is unchanged (`0`/`1`/`2`). |
| `selections` | `--json` | `{version, selections:[{name, path, owner, repoCount, digest, resolvedAt}]}`; `digest` is the short repo-set fingerprint (readable entries only); an unreadable entry carries an `error` field instead of being dropped; an empty registry is `selections: []`, not an error. |
| `workspaces` | `--json` | `{version, root, action, pruned, workspaces:[{path, purpose?, branch?, stamp?, owner?, sizeBytes, createdAt?, manifestPresent}]}` — `list` reports every snapshot, `prune` the matched subset (`pruned:true` once `--confirm` deleted them). See [`workspaces`](#goldfinger-workspaces). |
| `mirror` | `--report-json` | `{version, workspace, owner, repoCount, branch?, reconciliation:{inSelection, onDisk, notOnDisk, branch?:{present, fellBack, unknown}}, repos:[…]}` (see [`mirror`](#goldfinger-mirror)). |
| `apply` | `--plan-json` | `{version, dry_run, sign_mode, branch, pr_title, commit_message, pr_body_present, labels, reviewers, draft, batch_size, batch_pause, command_program, command_redacted, base_branch_source, repos:[{repo, base_branch_recorded}], repos_total}` — the invocation goldfinger will make, **not** the diff. See [`apply`](#goldfinger-apply). |
| `guide` | `--json` | `{version, commands:[{name, summary, requiredFlags, flags:[{name, usage, required, values?, default?}], example, notes?}]}` — a machine-consumable catalogue of the CLI surface, so an agent can discover every command, its flags, which are required, a flag's enum values, and a canonical example without parsing the prose playbook. Command names, flag names, and usage text are derived from the live command tree; requiredness, enum values, notes, and the example are curated and kept in sync with the validators by tests. |
| `schema` | *(always JSON)* | `{version, schemas:{lockfile, select, check, selections, doctor, apply-plan, mirror-report, capabilities, workspaces, workspace-manifest, error}}` — the [JSON Schema](https://json-schema.org/) (draft 2020-12) for the lockfile and every *other* payload in this table (including the machine-mode `error` object below), so a consumer can **validate** goldfinger's output rather than infer its shape. Read-only and offline: no token, no network, no git. The schemas are hand-authored but pinned to the Go types by a golden test and a reflection test, so they cannot silently drift. Where `guide --json` describes the *input* surface, `schema` describes the *output* surface. |

Each payload carries an explicit top-level `version` so consumers can branch on
shape across releases — the sole exception is `select --json`, whose version is
the nested `selection.version` (the lockfile version), so the nested object stays
structurally identical to the on-disk lockfile. `goldfinger schema` prints the
JSON Schema for every payload here (and the lockfile), so the shapes above are
machine-checkable, not just documented.

### Exit codes

goldfinger's exit status is a stable contract, so scripts and agents can branch
on outcome without scraping text:

| Code | Meaning | Examples |
| ---- | ------- | -------- |
| `0` | **Success** — the command did its job | in-sync `check`, a completed dry-run, a finished `mirror`, a written `select` |
| `1` | **A domain outcome, not a crash** — the command ran fine and is reporting a state you asked about | `check` found drift (the report is already on stdout; nothing on stderr); `doctor` had a failed check |
| `2` | **Error** — the command could not do its job | bad flags, no token / auth failure, a missing child tool, an unreadable lockfile, a zero-repo `select` without `--allow-empty`; `doctor` itself could not run |

`check` and `doctor` use `1` for a domain outcome (drift found / a failed
preflight check), so `if goldfinger check; then …` cleanly separates in-sync
(`0`) from drift (`1`) from error (`2`), and likewise `doctor` separates
all-clear from a failed check from a doctor that couldn't run. A wrong or
empty-result token trips `2`, never a false "in sync": `select` treats a
zero-repo match as an error (pass `--allow-empty` for the rare intended case)
rather than silently freezing an empty fleet.

**Failures are one parseable line, never a stack dump.** A genuine error is
always collapsed to a single line on stderr — even when a child tool's message
spanned several — so an agent never has to sift a traceback. In the human
default that line is `Error: <message>`; under `--quiet` it is instead the
compact machine object `{"version":1,"error":"<message>","exitCode":<n>}` (the
`error` surface in [`schema`](#goldfinger-schema)), so a machine reads one JSON
value plus the exit code. A domain-signal exit (drift `1`, a failed `doctor`
check) prints nothing — its report already went to stdout and the code carries
the signal — so a quiet run's stderr is empty unless something actually broke.

## Designed for agents: reducing token consumption

goldfinger's command streams are designed so an agent can keep only the data it
needs. stdout carries the result; stderr carries human progress, auth
announcements, banners, child-tool chatter, and summaries that can be discarded
with `2>/dev/null`. `--quiet` / `-q` suppresses that human stream and keeps stdout
to one representation:

```sh
selection=$(goldfinger select --quiet --org mycompany --topic platform)
workspace=$(goldfinger mirror --quiet --purpose keyv-cve)
goldfinger select --quiet --json --org mycompany --topic platform > selection-report.json
goldfinger mirror --quiet --report-json > mirror-report.json
```

Under quiet, `select` prints the lockfile path (or the JSON report with
`--json`), `mirror` prints the workspace path (or the JSON report with
`--report-json`; a dry-run creates no workspace, so it prints nothing), a dry-run
`apply` prints its per-repo status digest (or, with `--plan-json`, the plan JSON
instead), and `check`, `doctor`, `selections`, and `workspaces` print JSON only
when their `--json` flag is set. `guide` (prose) and `schema` are already stdout
payloads, so quiet does not change *what* they print — but where they emit JSON,
it still compacts it (below).

**Compact JSON in machine mode.** `--quiet` also switches every JSON payload from
the indented, human-readable form to compact single-line JSON — the same fields
and shape, just without the whitespace an agent pays tokens for. This applies to
every `--json`/`--report-json`/`--plan-json` surface plus `guide --json` and
`schema` (the two largest payloads, so the biggest saving). The default — no
`--quiet` — stays pretty-printed for a human terminal. Because only whitespace
differs, the [`schema`](#goldfinger-schema) contract is unchanged: output validated
against it in either form.

For apply dry-runs, consume goldfinger's per-repo status digest instead of
scraping the fleet-wide child output. The digest names `would-change`,
`no-change`, and `error` repos. In normal mode it prints to stderr with the full
captured output file path; under `--quiet` it becomes the stdout machine result
(and the temp file is skipped), so an agent still learns what would change
without the human banners — unless `--plan-json` is set, which then owns stdout
and suppresses the digest. A live (non-dry-run) quiet apply prints nothing; its
exit code carries multi-gitter's success.

Large artifacts are files, not streams: `mirror --write-report` writes the mirror
report under the workspace, and apply dry-runs write the full multi-gitter output
to a temp log. Machine contracts stay discoverable: `guide --json` is the
input catalogue, `schema` is the output contract, and command-specific flags like
`--report-json` and `--plan-json` keep stdout parseable.

## Install

goldfinger itself — the one-line installer grabs the right prebuilt binary for
your OS/arch, verifies its checksum, and installs it to `/usr/local/bin` (or
`~/.local/bin` if that isn't writable), printing a PATH hint if the target dir
isn't already on your PATH (no Go needed):

```sh
curl -sSfL https://raw.githubusercontent.com/redscaresu/goldfinger/main/install.sh | sh
# pin a version / install dir (env vars go before `sh`, so they reach the script):
#   curl -sSfL …/install.sh | GOLDFINGER_VERSION=v0.3.0 GOLDFINGER_BIN="$HOME/.local/bin" sh
goldfinger --version   # -> goldfinger version v0.3.0
```

Or, with Homebrew on macOS/Linux — this also pulls in `ghorg` and `multi-gitter`
automatically (they're `depends_on` in the formula), so there's nothing else to
install:

```sh
brew install redscaresu/tap/goldfinger
```

Prefer to fetch the binary yourself? (linux/darwin × amd64/arm64; macOS arm64 shown)

```sh
base=https://github.com/redscaresu/goldfinger/releases/latest/download
curl -sSfL -O "$base/goldfinger-darwin-arm64"
curl -sSfL -O "$base/goldfinger-darwin-arm64.sha256"
shasum -a 256 -c goldfinger-darwin-arm64.sha256   # verify BEFORE trusting the binary
chmod +x goldfinger-darwin-arm64
sudo mv goldfinger-darwin-arm64 /usr/local/bin/goldfinger
```

Keep the asset's own filename until after the check — the `.sha256` sidecar
records `<hash>  goldfinger-darwin-arm64`, so `shasum -c` looks for that exact
name; rename to `goldfinger` only on the final `mv` (the one-line installer above
does all of this for you). Browse builds at
[Releases](https://github.com/redscaresu/goldfinger/releases).

### Verify a release is built from this source (reproducible build)

The checksum above proves you downloaded exactly what the release *published*. It
does **not** prove that binary was built from the source in this repo. Because
the release build is deterministic — `CGO_ENABLED=0`, `-trimpath`, a fixed
`-ldflags`, and the Go toolchain pinned by go.mod's `go` directive — you can
close that gap yourself: rebuild the tag from source and confirm the hash
matches, trusting no build machine.

```sh
git clone https://github.com/redscaresu/goldfinger && cd goldfinger
git checkout <tag>             # the released tag — build from a CLEAN tree at this commit
make repro VERSION=<tag>       # rebuilds dist/goldfinger-<os>-<arch> and prints its sha256
```

The `repro` target ships from this change onward, so use a tag that includes it
(the first release after it landed, or later); older tags don't carry the target.

`make repro` uses the same toolchain and flags as the release build and clears
the ambient Go env that would otherwise change the bytes (`GOFLAGS`, `GOWORK`,
`GOEXPERIMENT`, `GOAMD64`), so on a standard Go install the only inputs are the
tag's source and the pinned toolchain. It forces that toolchain via
`GOTOOLCHAIN`, fetching it if your local Go differs — provided your bootstrap Go
is recent enough to support toolchain switching (Go 1.21+); an older one errors
out rather than fetching, so upgrade Go first. Compare its printed hash-and-name
line against the release's `goldfinger-<os>-<arch>.sha256` (or the line in
`SHA256SUMS`) — they match bit-for-bit. To verify a platform other than your
host, cross-build it:

```sh
make repro VERSION=<tag> GOOS=linux GOARCH=amd64
```

Two requirements make the hash reproduce: build from a **clean checkout of the
tag's commit** (the binary embeds the git revision, commit time, and a
clean/dirty flag — a modified tree or a different commit changes the bytes), and
let `make repro` pick the toolchain (don't override `GOTOOLCHAIN`). This proves
the published binary was built from this source; it is the source-trust
complement to the download checksum above.

Or install from source with Go:

```sh
go install github.com/redscaresu/goldfinger/cmd@v0.3.0   # or @latest
# The main package lives at ./cmd, so Go names the binary `cmd`. Rename it (Go
# installs to $GOBIN if set, otherwise $(go env GOPATH)/bin):
d="$(go env GOBIN)"; d="${d:-$(go env GOPATH)/bin}"; mv "$d/cmd" "$d/goldfinger"
# or build the repo (the Makefile already outputs a `goldfinger`-named binary)
git clone https://github.com/redscaresu/goldfinger && cd goldfinger && make build  # -> bin/goldfinger
```

The two tools goldfinger drives (the Homebrew install above already pulls these
in; only needed with the non-brew install methods — both Go, install with
`go install`):

```sh
go install github.com/gabrie30/ghorg@latest
go install github.com/lindell/multi-gitter@latest
# ensure your GOPATH bin is on PATH, e.g.:
export PATH="$PATH:$(go env GOPATH)/bin"
```

For auth, if you already use the GitHub CLI there's **nothing to set up** —
goldfinger picks up your `gh auth` session automatically (it shells out to
`gh auth token`). Log in once and you're done:

```sh
gh auth login   # grants `repo` scope by default, which is what goldfinger needs
```

`GOLD_FINGER_PAT` is the **fallback** for when no local gh login is available:
you don't have the gh CLI set up, or you're running in **CI** (no interactive
login — store the PAT as a secret and export it in the job). When set, it takes
precedence over the gh session. It needs Contents + Pull requests read/write:

```sh
export GOLD_FINGER_PAT=<a GitHub PAT with Contents + Pull requests read/write>
```

Either way you set one token; goldfinger maps it to the env vars ghorg and
multi-gitter each expect.

**Token precedence — and a footgun to know about.** goldfinger resolves its token
in this order:

1. `GOLD_FINGER_PAT` if set (explicit; the CI path).
2. otherwise `gh auth token` — your local gh session.

The catch: the `gh auth token` subprocess **itself** honours an ambient
`GH_TOKEN`/`GITHUB_TOKEN` in the environment, so a stray one of those can silently
change *which identity* goldfinger (and, downstream, ghorg) authenticates as —
producing a wrong-identity result that looks like "no repos found" rather than an
auth error. goldfinger surfaces this: unless `--quiet` is set, every
`select`/`check`/`mirror`/`apply` run prints its resolved token source and
authenticated principal on stderr, and warns when a gh-sourced token may be
shadowed by an ambient `GH_TOKEN`/`GITHUB_TOKEN`. If you see an unexpected
identity, `unset GITHUB_TOKEN GH_TOKEN` or set `GOLD_FINGER_PAT` explicitly. The
token value is never printed.

## Requirements

- **Go** — only to build goldfinger from source; the prebuilt release binaries
  need no Go.
- **ghorg** and **multi-gitter** on `PATH`. goldfinger checks for them and prints
  install instructions if missing.
- A **git identity** (`git config user.name` / `user.email`) — multi-gitter
  authors the `apply` commit from it.
- **A GitHub token** for API discovery — your `gh auth login` session by default,
  or `GOLD_FINGER_PAT` in CI. See [Install](#install) for the full auth setup.

## For AI agents

goldfinger is mostly operated by AI agents. `goldfinger guide` prints a compact
operator playbook (the workflow, the dry-run-by-default safety rule, examples)
that travels with the binary. The selection lockfile is JSON, and every error
names the next action — so an agent can self-orient and recover without this
README. Contributor-agent rules live in `AGENTS.md` and `CLAUDE.md`.

**Tell your own agents about it.** The way a tool becomes reachable to the agents
working in *your* repos is a line in their instructions file. Drop this into your
repo's `AGENTS.md` (read by Claude Code, Cursor, Codex, and others — or its
tool-specific equivalent like `CLAUDE.md`) so an agent reaches for goldfinger on
a fleet change instead of hand-rolling clone/PR loops:

```md
## Fleet-wide GitHub changes

For any change spanning many repos (bump a dependency, patch a CI config, rotate
a setting), use the `goldfinger` CLI rather than hand-rolling clone/PR loops — it
freezes the repo selection once and drives ghorg + multi-gitter against that exact
set. Run `goldfinger guide` for the playbook. Opening real PRs (`goldfinger apply
--dry-run=false --confirm`) needs explicit human authorization — always dry-run
first and present the status digest. Install: `brew install redscaresu/tap/goldfinger` or
`curl -sSfL https://raw.githubusercontent.com/redscaresu/goldfinger/main/install.sh | sh`.
```

## Development

```sh
make check   # go vet + race tests (mirrors CI's test job)
make e2e     # full-pipeline test against a sandbox repo (needs GOLD_FINGER_PAT + gh)
make hooks   # install the gitleaks pre-commit hook
```

CI runs the unit tests, gitleaks, govulncheck, and an end-to-end job that opens
and tears down a real PR on a sandbox repo. Contributor rules are in `AGENTS.md`
and `CLAUDE.md`.
