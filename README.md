# goldfinger

A CLI for SREs to manage GitHub repositories at scale: discover repos across an
org, clone them, apply a change to each one, and fan out pull requests — hundreds
at a time — without hand-rolling GitHub GraphQL queries, clone loops, or PR
scripts ever again.

## The problem

Fleet-wide changes (bumping a Docker base image, rotating a CI config, patching a
vulnerable dependency) currently mean stitching together the GitHub GraphQL API
for search, the REST API for PRs, and shell scripts for clone/commit/push. Every
SRE reinvents this pipeline, it's slow to write, easy to get wrong, and painful
to retry when 12 of 300 repos fail halfway through.

`goldfinger` replaces that with one tool:

```sh
# Bump a Docker image across every repo in the org that uses it
goldfinger run \
  --org mycompany \
  --branch bump-golang-1.24 \
  --commit-message "Bump golang base image to 1.24" \
  --pr-title "Bump golang base image to 1.24" \
  -- sed -i 's|golang:1.22|golang:1.24|g' Dockerfile
```

For each matching repo, goldfinger clones it, runs your script in the working
tree, and — if the script left the tree dirty — commits, pushes a branch, and
opens a PR. Repos the script doesn't change are skipped automatically.

It's fast because the heavy lifting happens over the git protocol — clone and
push are unmetered and parallelize freely — while the rate-limited GitHub API
is spent only on what git can't do: discovering repos and opening the PR.
GraphQL-scripted workflows route everything through one metered point budget;
goldfinger spends ~2–4 API calls per repo, total.

## Is Go the right language? Yes.

- **Single static binary.** SREs install one artifact — no Python venvs, no Node
  toolchain. Cross-compiles trivially for Linux/macOS/CI runners.
- **Concurrency is the core workload.** Fanning out over hundreds of repos is
  exactly what goroutines + `errgroup` + a semaphore are built for: bounded
  parallelism with clean cancellation and per-repo error collection.
- **First-class GitHub ecosystem.** `google/go-github` covers everything needed
  (repo listing, PR creation, rate-limit headers); `gh` itself is written in Go.
- **Prior art proves the model.** The closest existing tools —
  [multi-gitter](https://github.com/lindell/multi-gitter),
  [microplane](https://github.com/Clever/microplane), Sourcegraph batch changes —
  are all Go. This is well-trodden ground. (Worth evaluating multi-gitter before
  building: goldfinger's value over it is being tailored to our workflows, not
  novelty.)

## Design

### Interface

A single CLI binary with subcommands. Runs locally or in CI.

```
goldfinger repos    --org <org> [filters]        # discovery only: print matching repos
goldfinger run      --org <org> [filters] -- CMD # clone → run CMD → commit → push → PR
goldfinger status   --branch <name> --org <org>  # PR state across the fleet (open/merged/failed-ci)
goldfinger merge    --branch <name> --org <org>  # merge all green PRs for a campaign branch
goldfinger close    --branch <name> --org <org>  # abandon a campaign: close PRs, delete branches
```

### Repo targeting: org-wide discovery

Repos are discovered by enumerating the org via the REST API
(`GET /orgs/{org}/repos`, paginated), then filtered client-side:

- `--all-repos` — every non-archived repo in the org (explicit, never the default)
- `--topic <t>` — repeatable; match repos carrying any of the given topics
- Later: `--language`, `--contains-file`, `--repos-file` (explicit list, no discovery)

Discovery output is a plain list of `owner/name`, so `goldfinger repos` pipes
into `goldfinger run --repos-file -` for a review-then-execute workflow.

### Change model: script per repo

The change is an arbitrary command run inside each clone (the multi-gitter
model). This keeps goldfinger a pure engine — clone/branch/commit/push/PR —
while the transform itself is anything: `sed`, a Python script, `yq`, a compiled
tool. Contract:

- The command runs with CWD set to the repo's working tree.
- Environment gets `GOLDFINGER_REPO=owner/name` plus pass-through of the parent env.
- Exit non-zero → repo is marked **failed**, no commit is made.
- Exit zero with a clean tree → repo is **skipped** (no PR noise).
- Exit zero with a dirty tree → commit, push, PR.

### Execution engine

1. **Discover** matching repos (one paginated API pass).
2. **Fan out** with bounded concurrency (default ~10 workers, `--concurrency`).
   Each worker: shallow clone (`--depth 1`) into a temp dir → create branch →
   run script → commit if dirty → push → open PR via API → clean up the clone.
3. **Report** a per-repo result table: `success` (PR URL) / `skipped` / `failed`
   (captured script stderr), plus a machine-readable `--output json` for CI.

Git operations shell out to the system `git` binary rather than using go-git:
the git CLI is faster, battle-tested on edge cases (LFS, submodules, credential
helpers), and every target environment already has it. The GitHub API is only
used where git can't go: discovery, PR create/status/merge.

### Safety

- `--dry-run` — do everything except push and open PRs; print the diff per repo.
- `--interactive` — show each repo's diff and confirm before pushing.
- `--max-repos N` — hard cap per run; forces an explicit flag to go org-wide.
- Idempotent retries: if the campaign branch or PR already exists for a repo,
  update it instead of failing, so re-running a partially failed campaign only
  touches the stragglers.

### Auth and rate limits

- Auth via a PAT (classic or fine-grained) in `GITHUB_TOKEN` — never a flag, so
  tokens don't leak into shell history. The same token is used for the API and
  for git push (HTTPS with token credentials).
- Respect GitHub's rate-limit headers: back off on `403`/`429` with
  `Retry-After`, and honor secondary rate limits on PR creation (GitHub
  throttles rapid content creation — this is the practical ceiling on
  concurrency, not CPU).

### Non-goals (for now)

- No long-running service or state store — each run is stateless; `status`
  recomputes from the GitHub API using the campaign branch name as the key.
- No GitHub Enterprise Server support (github.com only) — but the API base URL
  will be a single config point so GHES is a small later change.
- No built-in transforms (e.g. a dedicated `bump-image` command). The script
  model covers it; add sugar later if a pattern proves common (rule of three).

## Proposed package layout

Flat top-level packages, one responsibility each (per
[simpleAPI](https://github.com/redscaresu/simpleAPI)), tests alongside the code:

```
cmd/        # main.go, CLI wiring (cobra)
discovery/  # org enumeration + filters (go-github)
campaign/   # the run engine: worker pool, per-repo pipeline
gitexec/    # thin wrapper over the git CLI
client/     # GitHub API: PR create/status/merge, rate-limit-aware
models/     # shared domain types: Repo, Campaign, RepoResult
```

## Roadmap

1. **MVP:** `repos` + `run` with dry-run, explicit repo list, PAT auth.
2. **Campaign lifecycle:** `status`, `merge`, `close`, idempotent re-runs.
3. **Richer discovery:** code-search-based targeting (find every repo whose
   Dockerfile references image X) to fully retire the GraphQL workflows.
4. **Later, if needed:** GitHub App auth for higher rate limits, GHES support,
   built-in transforms.
