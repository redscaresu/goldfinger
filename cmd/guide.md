goldfinger — operator guide

goldfinger is an orchestration layer. It resolves a set of GitHub repos once,
freezes them as a reviewable "selection" lockfile, then drives two external
tools against that exact set:
  - ghorg        mirrors the selection into a local workspace (clone/pull)
  - multi-gitter applies a change across the selection and opens PRs

goldfinger never writes to GitHub itself. Discovery is read-only; mirroring is
ghorg; commits/pushes/PRs are multi-gitter.

INSTALL
  brew install redscaresu/tap/goldfinger
  Homebrew pulls ghorg and multi-gitter in automatically (formula deps) and puts
  all three on PATH — so the "install ghorg and multi-gitter" prerequisite below
  is already satisfied. Non-brew: download a binary from the GitHub Releases page
  and install ghorg + multi-gitter yourself. Verify with `goldfinger --version`.

PREREQUISITES
  - Auth: if you're logged in with the GitHub CLI (gh auth login), goldfinger
    uses that session automatically — nothing to set. Otherwise (e.g. CI) set
    GOLD_FINGER_PAT to a GitHub PAT, which overrides the gh session when present.
    Either way goldfinger maps the one token to the env vars ghorg
    (GHORG_GITHUB_TOKEN) and multi-gitter (GITHUB_TOKEN) expect — one token, not
    three.
    Precedence: GOLD_FINGER_PAT if set, else `gh auth token`. FOOTGUN: the
    `gh auth token` subprocess itself honours an ambient GH_TOKEN/GITHUB_TOKEN,
    so a stray one silently changes which identity goldfinger (and ghorg)
    authenticate as — a wrong-identity run looks like "0 repos", not an auth
    error. Unless --quiet is set, every run prints its token source +
    authenticated principal on stderr and warns when a gh token may be shadowed;
    if the identity is wrong, `unset GITHUB_TOKEN GH_TOKEN` or set
    GOLD_FINGER_PAT. The token is never printed.
  - ghorg and multi-gitter on PATH (the brew install above pulls both in;
    install them yourself only for non-brew setups).
  - Configure a git identity (git config user.name / user.email). multi-gitter
    authors the apply commit from it; without it, apply silently makes no change
    and opens no PR.

WORKFLOW
  1. Select — resolve and freeze the repo set:
       goldfinger select --org <owner> --all-repos
       goldfinger select --org <owner> --topic platform --topic payments
     Writes ./goldfinger.selection (JSON: owner/name list + provenance). --org
     accepts a GitHub org OR user. stdout is terse by default — the count is on
     stderr and the full list is in the lockfile; add --list to echo every repo's
     full name on stdout, or --json for the full wrapper. The stderr done line
     ends with "(digest <hash>)": a short repo-set fingerprint (12 hex chars /
     48 bits over the sorted repo full-names, order-independent, set-only). It's
     a change detector, not a proof: a differing digest means the set definitely
     changed; a matching one is strong (truncated, so not absolute) evidence it's
     unchanged — a cheap check without diffing the lockfile. --json surfaces it
     as a top-level `digest`; `selections` shows it as a DIGEST column.

     If you plan to `mirror --branch <b>` (e.g. dev), add `--branch-presence <b>`
     here so goldfinger records (read-only) which repos actually have that branch
     and freezes it into the lockfile — the later mirror report then tells you
     which repos fall back to their default instead of silently missing the
     branch. These facts are recorded at selection time and can drift; re-select
     to refresh.

  2. Mirror — clone the selection locally (OPTIONAL — for reading/scanning the
     fleet; NOT needed to open PRs. apply clones on its own, see step 3):
       goldfinger mirror
     Repos land in <workspace>/<owner> (default workspace ~/goldfinger).
     Re-run any time to refresh (ghorg pulls existing clones).

     When ghorg finishes, goldfinger prints its own reconciliation line, e.g.
       ✓ reconciliation: in selection: 59 | on disk: 59 | branch present: 15 | fell back: 44
     Read THIS, not ghorg's "N new clones" — ghorg counts only *newly* cloned
     repos, so a re-mirror of an unchanged fleet says "0 new clones" while all 59
     are present. "in selection" is the lockfile count; "on disk" is a read-only
     count of how many of those repos actually landed under <workspace>/<owner>
     (no git). If on disk < in selection, goldfinger warns (⚠) that the mirror
     under-covered the selection. With --branch, "branch present"/"fell back"
     (and "unknown", when any) explain ghorg's per-repo "Could not checkout
     <branch>" lines as expected fall-backs, not failures — same facts as the
     --report-json branchStatus below.

     ghorg's output still streams live to stderr, but goldfinger also captures
     the full run to a 0600 temp log and prints its path
       ✓ ghorg output captured at /var/folders/.../goldfinger-mirror-output-*.log
     so you can drill into clone errors behind a shortfall without scrolling
     back (on a failed mirror the same path is surfaced with ⚠). Under --quiet
     the output is discarded — no live stream, no log.

     Pass --branch <name> to check out a specific branch in every clone instead
     of each repo's default. It is ONE name applied to all repos: ghorg leaves a
     repo on its default branch where that branch is absent (best-effort "prefer
     dev where it exists", not a per-repo guarantee).

     Do NOT combine --branch with --clone-depth: a shallow clone
     (--clone-depth 1) fetches only each repo's default branch, so
     `mirror --branch dev --clone-depth 1` would leave repos on their default
     wherever dev exists but isn't the default — a silent coverage gap.
     goldfinger refuses that combination; omit --clone-depth (full depth) when
     you pass --branch. Shallow is fine for a plain default-branch scan.

     To see which repos got the branch vs fell back, add --report-json (prints a
     JSON report to stdout instead of the bare workspace-path line) or
     --write-report (writes <workspace>/goldfinger-mirror.json, only on a
     successful mirror). The report is built from the lockfile alone (no git, no
     re-discovery): it lists each repo's branchStatus as has-branch /
     falls-back-to-default / unknown. "unknown" means the branch wasn't checked
     at select time (run select --branch-presence <b> first) — goldfinger does
     NOT guess. Branch facts are recorded at selection time and can drift.

     For a one-off mass-PR campaign, use --purpose for an ephemeral, timestamped
     workspace: you supply the purpose, goldfinger stamps the time to the
     millisecond so each run gets its own pristine dir —
       goldfinger mirror --purpose keyv-cve --clone-depth 1
       # clones into ~/goldfinger/keyv-cve-2026-08-04-132045.123/<owner>/
       # ...scan / develop the change script against that snapshot...
     With --branch the branch is folded into the name too, <purpose>-<branch>-<stamp>
     (a branch's slashes become dashes) —
       goldfinger mirror --purpose keyv-cve --branch dev
       # clones into ~/goldfinger/keyv-cve-dev-2026-08-04-132045.123/<owner>/
     A --purpose mirror also drops a small sidecar manifest at the snapshot root
     (goldfinger-workspace.json: purpose, branch, stamp, owner, createdAt) so
     `workspaces list/prune` (below) get reliable structured metadata — the dir
     name alone can't be split back into its parts (purpose and a sanitised
     branch can both contain '-'). The manifest is written only on a successful,
     non-dry-run mirror.
     goldfinger NEVER deletes the directory — it persists so you can review it;
     reclaim old snapshots with `goldfinger workspaces prune` (below), or just
     rm -rf the dir yourself when done
     (e.g. rm -rf ~/goldfinger/keyv-cve-2026-08-04-132045.123).
     A fresh per-campaign clone is pristine by construction, so it never hits the
     divergence trap a long-lived clone can: if upstream rebases/squashes its
     default branch, a stale persistent clone can no longer git pull (exit 128) —
     git clean removes files, not commits, so it cannot recover a diverged clone.
     Keep the durable artifact (the lockfile), not the clones.

     The lockfile is authoritative: goldfinger strips set-narrowing/pruning
     GHORG_* env vars and forces an empty ghorgignore, and invokes multi-gitter
     with an empty --config, so ambient host config can't change the set.

  3. Apply — run a change across the selection and open PRs:
       goldfinger apply --branch bump --commit-message "msg" --pr-title "title" \
         --sign local -- sed -i 's|old|new|g' Dockerfile
     The command after -- runs in each repo's checkout (via multi-gitter), on
     your machine — keep it portable (`sed -i` differs on macOS/BSD). For
     non-trivial or per-file edits, pass a script: `-- python3 /abs/migrate.py`.
     If it changes files and exits 0, a PR is prepared. multi-gitter makes its
     own temporary checkout per repo (NOT the mirror workspace), so apply is
     independent of step 2 — you can apply without ever running mirror. It
     branches from the base branch's LIVE HEAD at apply time, not the SHA you
     mirrored in step 2 — so always --dry-run first (it clones fresh too) to see
     which repos would change, report no change, or error rather than trusting
     the snapshot you inspected. Non-interactive multi-gitter dry-run does NOT
     emit a unified diff; goldfinger prints a status digest plus a full-output
     file path.
     With --base-branch omitted, each PR targets that repo's own default branch,
     so a mixed dev/main selection routes correctly per repo.

SIGNING (--sign, REQUIRED — no default)
  Every apply must state how commits are signed. There is no default on purpose:
  commit provenance is too important to leave implicit for a fleet-wide change.
  Three modes, three trust models — the dry-run banner spells out which one is
  in effect:
  - --sign local  : real git binary (multi-gitter --git-type=cmd) → signed with
                     YOUR GPG key, honouring ~/.gitconfig commit.gpgsign /
                     user.signingkey. "Verified" on GitHub only if that public
                     key is uploaded. Your gpg-agent must have the passphrase
                     cached for the whole run — a cold/headless agent can stall
                     on per-commit pinentry, so warm it first. (--git-type=cmd
                     runs `git commit` with no -S and no --no-gpg-sign, so your
                     commit.gpgsign config is honoured — verified against
                     multi-gitter v0.63.1.)
  - --sign github : GitHub API push (multi-gitter --api-push) → signed by
                     GitHub's web-flow key, always "Verified", no local key.
                     GitHub-only, slower, unsuited to large files.
  - --sign none   : UNSIGNED (multi-gitter default go-git). Explicit opt-out.

PREFLIGHT (doctor)
  Before your first run on a machine (or in CI), confirm the environment:
       goldfinger doctor
       goldfinger doctor --json
  It is entirely read-only — never writes GitHub, never runs git, never prints the
  token — and reports one line per check:
    - auth         : which token SOURCE and GitHub PRINCIPAL a run would use
                     (authenticated as <login> via GOLD_FINGER_PAT / gh session).
    - auth-shadow  : warns if an ambient GITHUB_TOKEN/GH_TOKEN may be shadowing
                     your gh login (the wrong-identity footgun above).
    - ghorg /      : each child tool's PATH location + version, or a fail with an
      multi-gitter   install hint if missing.
    - git-identity : user.name/user.email resolved from system+global+env config
                     (NOT via git — parsed directly). A warn if unset, because
                     apply would then silently make no commit.
    - signing      : commit.gpgsign / user.signingkey readiness for `--sign local`
                     (advisory only — github/none don't need local config).
  Exit status: 0 nothing failed, 1 a check failed (no token, a missing child
  tool), 2 doctor itself could not run. Warns and info never fail the run, so
  `goldfinger doctor` is a safe CI gate for "can this box run goldfinger at all".

DRIFT CHECK
  A selection is frozen at select time; the world moves on. Before a big mirror
  or apply, confirm the lockfile still matches reality:
       goldfinger check
       goldfinger check --name platform
  It re-runs discovery using the selection's OWN recorded filter and diffs the
  result against the lockfile — reporting repos added (+), removed with a reason
  (-), whose default branch has moved (~), or whose owner type has flipped (!).
  It is read-only: it never rewrites the lockfile (re-run `select` to refresh)
  and never re-runs discovery inside mirror/apply. Exit status makes it a CI
  gate: 0 in sync, 1 drift found, 2 error.

NAMED SELECTIONS
  By default a selection is ./goldfinger.selection. To keep several standing
  cohorts, name them: `select --name platform ...` stores it in a registry
  (~/.config/goldfinger/selections/<name>.json). Then `mirror --name platform`
  or `apply --name platform ...`. `goldfinger selections` lists them. --name and
  --selection are mutually exclusive.

WORKSPACE LIFECYCLE (workspaces list | prune)
  Every `mirror --purpose` leaves a timestamped snapshot dir under the workspace
  root (default ~/goldfinger); goldfinger never deletes them for you, so they
  accumulate. `workspaces` is the safe, first-class way to see and reclaim them.
  It is filesystem-only: it never touches GitHub and runs no git.
       goldfinger workspaces list          # size + creation time per snapshot
       goldfinger workspaces list --json   # {version, root, action, workspaces:[…]}
  list enumerates only the stamped snapshot dirs (those ending -<stamp>); the
  default ~/goldfinger/<owner> mirror and any unrelated dir are ignored. A
  snapshot's purpose/branch/owner come from its sidecar manifest when present;
  a legacy manifest-less snapshot is still listed (createdAt recovered from the
  dir-name stamp) but with no structured purpose/branch — the dir name is NOT
  reliably splittable, so don't parse it.
       goldfinger workspaces prune                      # PREVIEW: shows what it
                                                        # would remove, deletes 0
       goldfinger workspaces prune --confirm            # actually delete
       goldfinger workspaces prune --older-than 7d      # only snapshots >7d old
       goldfinger workspaces prune --purpose keyv-cve   # only that purpose
  prune mirrors apply's posture: it PREVIEWS by default and deletes only with
  --confirm — it never removes a snapshot on its own. Narrow with --older-than
  <dur> and/or --purpose <name>; --older-than takes day/week sugar (7d, 2w) as
  well as any Go duration (168h, 90m). With no filter it targets every snapshot
  (still
  confirm-gated). Both filters are conservative: --purpose matches only
  manifest-tagged snapshots whose recorded purpose is EXACTLY that name (a
  manifest-less snapshot is never matched by purpose), and --older-than skips any
  snapshot whose age can't be determined — an ambiguous snapshot is kept, not
  deleted. prune only ever removes a stamp-suffixed dir directly under the root,
  re-checked immediately before each delete. --older-than/--purpose/--confirm
  apply to prune only; passing them to list is an error. There is no time-based
  auto-GC — deletion is always an explicit, confirmed action.

  Not yet available: a single-branch, full-depth clone (one branch, full history)
  to cut mirror size without the shallow trap. ghorg (as of v1.11.14) exposes no
  single-branch flag, and goldfinger will not reimplement cloning, so this is
  deferred pending an upstream ghorg option. For now: --clone-depth 1 (shallow,
  default branch only) is the size lever; omit it for a full clone.

SAFETY — READ THIS
  - `apply` defaults to --dry-run: it prints a per-repo status digest and opens NOTHING.
  - A real run needs BOTH --dry-run=false AND --confirm. This is deliberate.
  - If you are an AI agent: never run a real (non-dry-run) apply on your own
    initiative. When the human has explicitly authorized this fleet change, you
    may run it — but dry-run first, present the status digest, prefer --draft
    (PRs open not-ready-for-review), and pass --dry-run=false --confirm.
    Otherwise present the dry-run result and let the human run the real apply
    themselves.
  - --sign is required on every run: pass it explicitly and state which trust
    model you used (local = your GPG key, github = GitHub's key, none = unsigned)
    when you present the dry-run or a real run.

EXIT CODES
  goldfinger's exit status is a stable contract you can branch on in scripts:
    0  success — the command did its job. In-sync `check`, a completed dry-run,
       a finished mirror, a written selection.
    1  a domain OUTCOME, not a crash — the command ran fine but is reporting a
       state you asked about. `check` uses it when drift was found (the drift
       report is already on stdout; nothing is printed to stderr); `doctor` uses
       it when a preflight check failed. Treat 1 as "answer = yes/drift/failed
       check", not "it broke".
    2  ERROR — bad flags, no token / auth failure, a missing child tool, an
       unreadable lockfile, or a zero-repo `select` without --allow-empty; also
       `doctor` itself could not run. The message on stderr names the next action.
  So: `if goldfinger check; then ...` distinguishes 0 (sync) from 1 (drift) from
  2 (error), and `goldfinger doctor` likewise separates all-clear (0) from a
  failed check (1) from a doctor that couldn't run (2) — a wrong token trips 2,
  never a false "in sync".

MACHINE-READABLE OUTPUT
  Every read command emits JSON on request, one contract: stdout = machine data,
  stderr = human banners/logs. In --json mode stdout is ONLY the JSON. Human
  stderr can be discarded with 2>/dev/null. The global --quiet / -q flag silences
  that human stream and keeps stdout to a single machine result; it also emits
  every JSON payload compact (single-line) rather than indented, so an agent
  parsing it spends fewer tokens. The default (no --quiet) stays pretty-printed
  for a human terminal. Only whitespace differs — the shape is identical.
       goldfinger select --json ...   -> {selectionPath, selection:{…lockfile…}, digest}
       goldfinger doctor --json       -> {version, checks:[{check, status, detail, fix}]}
       goldfinger check --json        -> {version, inSync, added, removed, …}
       goldfinger selections --json   -> {version, selections:[{name, owner, …}]}
       goldfinger workspaces list --json -> {version, root, action, workspaces:[…]}
       goldfinger mirror --report-json -> {version, workspace, owner, reconciliation:{inSelection, onDisk, notOnDisk, branch?}, repos, …}
       goldfinger apply --plan-json ... -> {version, dry_run, sign_mode, repos, …}
       goldfinger guide --json        -> {version, commands:[{name, flags, …}]}
       goldfinger schema              -> {version, schemas:{lockfile, check, …}}
  Quiet non-JSON stdout contract:
       goldfinger select --quiet ...   -> lockfile path
       goldfinger mirror --quiet ...   -> workspace path (empty for a dry-run)
       goldfinger apply --quiet ...    -> dry-run status digest on stdout (no temp
                                          file); --plan-json instead -> plan JSON,
                                          digest suppressed; a live run -> empty
       goldfinger check --quiet        -> empty stdout; exit code carries sync/drift
       goldfinger doctor --quiet       -> empty stdout; exit code carries pass/fail
       goldfinger selections --quiet   -> empty stdout unless --json
       goldfinger workspaces list --quiet -> empty stdout unless --json
  guide (prose) and schema are already stdout payloads, so --quiet does not
  change WHAT they print — but where they emit JSON (guide --json, and schema,
  which is always JSON), --quiet still compacts it to a single line.
  guide --json is the self-describing CLI catalogue: every command, its flags,
  which flags are required, a flag's enum values (e.g. --sign), and a canonical
  example per command — discover what goldfinger can do by parsing structure
  instead of this prose. Names/usage come from the live command tree; requiredness
  and enums are kept in sync with the validators by tests.
  schema is the output-side companion: it prints the JSON Schema (draft 2020-12)
  for the lockfile and every payload above, so you can VALIDATE what goldfinger
  emits — or hand a validator the exact shape — instead of inferring it. It is
  read-only and offline (no token, no network, no git), and its schemas are pinned
  to the Go types by a golden test, so they cannot drift. Where guide --json
  describes the INPUT surface, schema describes the OUTPUT surface.
  apply --plan-json emits the INVOCATION plan (not the diff; command redacted to
  argv[0], body as a presence bool) on stdout and STILL runs multi-gitter's
  dry-run — you get both the plan and the status digest plus full-output file
  path (on stderr). Under --quiet the human stderr is silenced: --plan-json then
  yields the plan alone on stdout, or without it the status digest moves to
  stdout (and the full-output temp file is not written).
  Each payload carries a top-level `version` for shape-stability, except
  `select --json` whose version is the nested selection.version (the lockfile
  version), so the nested object stays identical to goldfinger.selection on disk.
  Failures are one parseable line, never a stack dump: a genuine error collapses
  to a single stderr line — `Error: <msg>` in human mode, or the compact object
  {version, error, exitCode} under --quiet (the `error` surface in schema). A
  domain-signal exit (drift, a failed doctor check) prints nothing; the exit code
  carries it (0 ok / 1 domain outcome / 2 error).

NOTES FOR AI AGENTS
  - The selection lockfile is JSON — read it directly for structured state.
  - Run `goldfinger doctor --json` first on an unfamiliar box: it tells you the
    principal, whether the child tools are present, and whether apply will commit
    — a machine-readable go/no-go before you select or apply.
  - Every read command takes --json (doctor/select/check/selections) or --report-json
    (mirror): prefer it over scraping prose. stdout is the data, stderr the noise.
    Add --quiet / -q when you want stderr silenced and stdout reduced to the
    single machine result (or JSON when a JSON flag is also set).
  - `goldfinger schema` prints the JSON Schema for every one of those payloads —
    validate goldfinger's output against it rather than guessing field shapes.
  - Before authoring an apply, MIRROR first and READ the real code (Dockerfiles,
    imports, CI configs, etc.). A fleet change script written blind will be wrong
    on the edge cases — the variety across repos is exactly why you inspect a
    local snapshot before fanning out. Mirror with --purpose <name> for an
    ephemeral, timestamped snapshot, read + develop+test the script there, then
    apply (which clones its own copy).
  - Every error names the next action (e.g. "run goldfinger select first",
    or an install hint for a missing tool). Follow it.
  - "the repos I mirror" and "the repos I apply to" are the same frozen set,
    from one lockfile — that is the guarantee goldfinger exists to provide.
