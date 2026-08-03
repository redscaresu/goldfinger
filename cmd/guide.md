goldfinger — operator guide

goldfinger is an orchestration layer. It resolves a set of GitHub repos once,
freezes them as a reviewable "selection" lockfile, then drives two external
tools against that exact set:
  - ghorg        mirrors the selection into a local workspace (clone/pull)
  - multi-gitter applies a change across the selection and opens PRs

goldfinger never writes to GitHub itself. Discovery is read-only; mirroring is
ghorg; commits/pushes/PRs are multi-gitter.

PREREQUISITES
  - Auth: if you're logged in with the GitHub CLI (gh auth login), goldfinger
    uses that session automatically — nothing to set. Otherwise (e.g. CI) set
    GOLD_FINGER_PAT to a GitHub PAT, which overrides the gh session when present.
    Either way goldfinger maps the one token to the env vars ghorg
    (GHORG_GITHUB_TOKEN) and multi-gitter (GITHUB_TOKEN) expect — one token, not
    three.
  - Install ghorg and multi-gitter and put them on PATH.
  - Configure a git identity (git config user.name / user.email). multi-gitter
    authors the apply commit from it; without it, apply silently makes no change
    and opens no PR.

WORKFLOW
  1. Select — resolve and freeze the repo set:
       goldfinger select --org <owner> --all-repos
       goldfinger select --org <owner> --topic platform --topic payments
     Writes ./goldfinger.selection (JSON: owner/name list + provenance) and
     prints the set. --org accepts a GitHub org OR user.

  2. Mirror — clone the selection locally (optional; for grep/inspection):
       goldfinger mirror
     Repos land in <workspace>/<owner> (default workspace ~/goldfinger).
     Re-run any time to refresh (ghorg pulls existing clones).
     The lockfile is authoritative: goldfinger strips set-narrowing/pruning
     GHORG_* env vars and forces an empty ghorgignore, and invokes multi-gitter
     with an empty --config, so ambient host config can't change the set.

  3. Apply — run a change across the selection and open PRs:
       goldfinger apply --branch bump --commit-message "msg" --pr-title "title" \
         -- sed -i 's|old|new|g' Dockerfile
     The command after -- runs in each repo's checkout (via multi-gitter), on
     your machine — keep it portable (`sed -i` differs on macOS/BSD). For
     non-trivial or per-file edits, pass a script: `-- python3 /abs/migrate.py`.
     If it changes files and exits 0, a PR is prepared.
     With --base-branch omitted, each PR targets that repo's own default branch,
     so a mixed dev/main selection routes correctly per repo.

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

NOTES FOR AI AGENTS
  - The selection lockfile is JSON — read it directly for structured state.
  - Every error names the next action (e.g. "run goldfinger select first",
    or an install hint for a missing tool). Follow it.
  - "the repos I mirror" and "the repos I apply to" are the same frozen set,
    from one lockfile — that is the guarantee goldfinger exists to provide.
