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
  - ghorg and multi-gitter on PATH (the brew install above pulls both in;
    install them yourself only for non-brew setups).
  - Configure a git identity (git config user.name / user.email). multi-gitter
    authors the apply commit from it; without it, apply silently makes no change
    and opens no PR.

WORKFLOW
  1. Select — resolve and freeze the repo set:
       goldfinger select --org <owner> --all-repos
       goldfinger select --org <owner> --topic platform --topic payments
     Writes ./goldfinger.selection (JSON: owner/name list + provenance) and
     prints the set. --org accepts a GitHub org OR user.

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
     goldfinger NEVER deletes the directory — it persists so you can review it;
     clean it up yourself when done
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
     the real diff rather than trusting the snapshot you inspected.
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

SAFETY — READ THIS
  - `apply` defaults to --dry-run: it shows the planned change and opens NOTHING.
  - A real run needs BOTH --dry-run=false AND --confirm. This is deliberate.
  - If you are an AI agent: never run a real (non-dry-run) apply on your own
    initiative. When the human has explicitly authorized this fleet change, you
    may run it — but dry-run first, present the diff, prefer --draft (PRs open
    not-ready-for-review), and pass --dry-run=false --confirm. Otherwise present
    the dry-run result and let the human run the real apply themselves.
  - --sign is required on every run: pass it explicitly and state which trust
    model you used (local = your GPG key, github = GitHub's key, none = unsigned)
    when you present the dry-run or a real run.

NOTES FOR AI AGENTS
  - The selection lockfile is JSON — read it directly for structured state.
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
