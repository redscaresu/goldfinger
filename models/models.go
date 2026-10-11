// Package models holds the domain types shared across goldfinger's packages.
// It has no dependencies on other goldfinger packages.
package models

import "time"

// Repo is a GitHub repository in a selection.
type Repo struct {
	Owner         string   `json:"owner"`
	Name          string   `json:"name"`
	CloneURL      string   `json:"cloneURL"`
	DefaultBranch string   `json:"defaultBranch"`
	Topics        []string `json:"topics,omitempty"`
	Archived      bool     `json:"archived,omitempty"`

	// BranchPresence records, per branch name checked at selection time via
	// read-only REST (`select --branch-presence`), whether that branch existed on
	// this repo. It is a frozen fact recorded at selection time and can drift
	// afterwards; an absent entry means the branch was never checked, so callers
	// must treat it as "unknown" and never guess.
	BranchPresence map[string]bool `json:"branchPresence,omitempty"`
}

// FullName returns the canonical "owner/name" identifier.
func (r Repo) FullName() string {
	return r.Owner + "/" + r.Name
}

// RecordedBranch reports what the selection knows about branch on this repo.
// known is false when the branch was not checked at selection time (an old v1
// lockfile, or a branch never passed to `select --branch-presence`) — callers
// must not guess in that case. has is whether the branch was present. A branch
// equal to the repo's own DefaultBranch is always present and known without any
// recorded fact.
func (r Repo) RecordedBranch(branch string) (has, known bool) {
	if branch != "" && branch == r.DefaultBranch {
		return true, true
	}
	has, known = r.BranchPresence[branch]
	return has, known
}

// TokenEnvVar is the environment variable goldfinger reads the operator's
// GitHub PAT from. goldfinger maps it onto each child tool's own token variable
// (GITHUB_TOKEN, GHORG_GITHUB_TOKEN) and strips it from the child environment,
// so the raw PAT never reaches a delegate or a user-supplied apply script under
// this name.
const TokenEnvVar = "GOLD_FINGER_PAT"

// The token variables the delegates read. Naming them here — rather than as
// literals in apply/ and mirror/ — is what makes each delegate's own variable
// provably a member of credentialEnvVars below, so the variable a delegate is
// handed is always one the scrub covers.
const (
	// MultiGitterTokenEnvVar is the environment variable multi-gitter reads its
	// GitHub token from; `apply` maps the operator's PAT onto it.
	MultiGitterTokenEnvVar = "GITHUB_TOKEN" //nolint:gosec // G101: this is the name of an env var, not a hardcoded credential.
	// GhorgTokenEnvVar is the environment variable ghorg reads its GitHub token
	// from; `mirror` maps the operator's PAT onto it.
	GhorgTokenEnvVar = "GHORG_GITHUB_TOKEN" //nolint:gosec // G101: this is the name of an env var, not a hardcoded credential.
	// GHTokenEnvVar is gh's own token variable. goldfinger never sets it, but it
	// is scrubbed from every delegate environment: gh resolves GH_TOKEN AHEAD of
	// GITHUB_TOKEN, so an ambient one would make an apply script that shells out
	// to gh authenticate as a different account than the one goldfinger resolved
	// and announced.
	GHTokenEnvVar = "GH_TOKEN" //nolint:gosec // G101: this is the name of an env var, not a hardcoded credential.
)

// credentialEnvVars is the canonical set of environment variables that can carry
// a GitHub credential — or the means to fetch one — into a child process. Every
// delegate environment goldfinger builds strips ALL of them and then sets back
// only the single variable that delegate is meant to see, so the GitHub identity
// goldfinger announces and the one a child acts as (ghorg, multi-gitter, or the
// operator's own apply script, which multi-gitter runs as a grandchild) are the
// same by construction rather than by the operator remembering to unset things.
// It is unexported so no other package can shrink the set the scrub trusts — an
// exported slice could be re-sliced or overwritten to defeat it.
//
// The scope is GitHub credentials, deliberately: the apply script is the
// operator's own program and inherits the rest of their environment, so this list
// is not a general secrets filter and does not pretend to be one.
var credentialEnvVars = []string{
	TokenEnvVar,
	MultiGitterTokenEnvVar,
	GhorgTokenEnvVar,
	GHTokenEnvVar,
	// ghorg's GitHub App authentication (`ghorg clone --help`, v1.11.14). These
	// are an ALTERNATIVE identity, not the token goldfinger resolved: an ambient
	// set would let ghorg clone as an App installation while goldfinger announces
	// a PAT principal — the same announced-identity-is-not-acting-identity defect
	// the token scrub exists to prevent, just via a different credential. The PEM
	// path additionally points a child at a private key file that no delegate
	// goldfinger invokes has any use for. multi-gitter has no equivalent (v0.63.1
	// reads only GITHUB_TOKEN for GitHub), but it costs nothing to scrub them
	// from its environment too.
	"GHORG_GITHUB_APP_ID",
	"GHORG_GITHUB_APP_INSTALLATION_ID",
	"GHORG_GITHUB_APP_PEM_PATH",
	"GHORG_GITHUB_TOKEN_FROM_GITHUB_APP",
	// ghorg will SOURCE a token by running this as `sh -c` and using its stdout
	// (configs.runTokenCmd, v1.11.14) — a credential the operator never handed
	// goldfinger, obtained by a command the host chose. goldfinger's mapped token
	// already disarms it, because ghorg skips the command whenever a token is
	// already set for the active SCM, so this is the second lock and not the
	// first; it is here because a credential-sourcing shell command should not be
	// one empty token away from running inside a child goldfinger built.
	"GHORG_TOKEN_CMD", //nolint:gosec // G101: this is the name of an env var, not a hardcoded credential.
}

// CredentialEnvVars returns a fresh copy of the credential-bearing environment
// variables, for callers building a child environment (apply, mirror) or probing
// a child tool (doctor). A copy, so a caller holding the result cannot mutate the
// canonical list.
func CredentialEnvVars() []string {
	out := make([]string, len(credentialEnvVars))
	copy(out, credentialEnvVars)
	return out
}

// gitGrandchildEnvVars are git's OWN environment variables, scrubbed from BOTH
// delegates' environments even though neither delegate reads them.
//
// The reason is that git is a GRANDCHILD, not a child. ghorg execs `git` for every
// clone and never sets cmd.Env (19 call sites in git/git.go, zero Env
// assignments), so git inherits whatever goldfinger passed ghorg; multi-gitter
// does the same under --git-type=cmd, which is exactly what --sign=local selects
// (internal/git/cmdgit/git.go:66). Both put the PAT in the clone URL before
// handing it to git — ghorg at scm/github.go:188 (addTokenToHTTPSCloneURL, taken
// because goldfinger pins --protocol=https), multi-gitter at
// internal/scm/github/repository.go:20-21. So the resolved token is sitting in
// git's argv, and git has knobs that read argv and knobs that rewrite it.
//
// This is the one surface where the two delegates are SYMMETRIC, which is worth
// stating because everywhere else in this codebase they are not. multi-gitter's
// CensorFormatter (cmd/logging.go:69-78) rewrites the token to "<TOKEN>" in every
// line multi-gitter itself logs, and that is why no multi-gitter log knob is
// occupied — but it cannot censor bytes it never sees, and git writes these files
// itself. Neither delegate's own redaction reaches here.
//
// The list below is the result of classifying EVERY git environment variable, not
// of reacting to the ones a reviewer happened to name. The population is the union
// of the names in git(1)'s ENVIRONMENT VARIABLES section and the GIT_* strings in
// the shipped binary (git 2.50.1) — 176 names. Each was placed in one of the four
// scrubbed families below or in one of the excluded groups after it, so a later
// reviewer checks the CLASSIFICATION rather than re-enumerating; a new git release
// adds names to the population, and the question to ask of each is only which group
// it belongs to.
//
// The inclusion rule, stated once so it can be applied rather than remembered: a
// variable is scrubbed if it can disclose the token, redirect where the token or the
// push goes, change which repository or objects git acts on, or change what content
// or identity ends up in the clone or the commit — AND its removal is monotonically
// safer. That second half is what keeps hardening switches out; see the excluded
// groups.
//
// Four families, each verified against a real git rather than from the docs:
//
//   - The TRACING family DISCLOSES the token. GIT_TRACE prints argv, and set to an
//     absolute path it appends to that file — so a hostile or merely stale
//     GIT_TRACE=/tmp/x writes the live credential-bearing clone URL to a location
//     the host chose, once per repo. GIT_TRACE2_ENV_VARS additionally prints named
//     variables' values, and GIT_TRACE_REDACT=0 switches OFF the redaction git
//     would otherwise apply to Authorization headers.
//
//   - The REDIRECTION family sends the token somewhere goldfinger never resolved.
//     Its core is config injection. GIT_CONFIG_COUNT with
//     GIT_CONFIG_KEY_n/GIT_CONFIG_VALUE_n injects arbitrary config into every git
//     invocation, and `url.<evil>.insteadOf` rewrites the remote — sending the PAT
//     to a host goldfinger never resolved and never announced, which is invariant
//     (a) failing in the most direct way available. The prefix insteadOf matches on
//     is not even a guess: both delegates use a FIXED username in the URL
//     (multi-gitter "oauth2", ghorg's tokenUsername), so "https://oauth2:" is a
//     literal an attacker can write down. GIT_CONFIG_PARAMETERS is the same attack
//     through git's own propagation channel — it is how `git -c k=v` reaches a
//     subprocess, so a stale one is as likely as a hostile one, and git ranks it at
//     "command line" precedence, above every file. Verified: with it set, the same
//     insteadOf rule rewrote an https://oauth2:<token>@github.com/... remote.
//     GIT_SSL_NO_VERIFY belongs to the same family by effect rather than by
//     mechanism: it does not rewrite the remote, it stops git checking that the
//     remote is who it claims to be, which is what turns a proxy in front of the
//     clone into a credential interception. It is also the clearest case of the
//     monotonic half of the rule, because git tests the variable's PRESENCE and
//     never its value — verified, GIT_SSL_NO_VERIFY=false got past a self-signed
//     certificate that the same command rejected with the variable unset. So no
//     value of it can make a run safer, and an operator who needs a private CA has
//     http.sslCAInfo for that; one who genuinely needs verification off would
//     already be failing in goldfinger's own REST client, which is Go's TLS stack
//     and has never honoured this variable.
//
//   - The REPO-TARGET family RETARGETS git away from the clone the delegate chose.
//     Both delegates locate the repository purely by DIRECTORY — ghorg sets cmd.Dir
//     on every git call (git/git.go:62 onward) and multi-gitter sets it centrally
//     (internal/git/cmdgit/git.go:28) — and neither ever sets these itself, so an
//     inherited one silently outranks the directory. git also exports some of them
//     into hook environments, which is the realistic route: a goldfinger run
//     launched from a hook inherits them without anyone being hostile. Each was
//     verified by reproducing the delegate's own command shape:
//
//     GIT_DIR — an add/commit/push in multi-gitter's shape ran against the
//     operator's OWN repository and pushed its private commit object to the remote,
//     reachable from the branch a PR would be opened on. GIT_WORK_TREE — the
//     commit's tree was the content of an unrelated directory and the change
//     command's actual output was dropped. GIT_INDEX_FILE — ghorg's
//     `git clean -f -d` (git/git.go:206) deleted every tracked file from the clone,
//     because a foreign index says they are untracked; the mirror is then empty
//     while the directory still exists, and reconciliation counts directories.
//     GIT_OBJECT_DIRECTORY — new objects landed in a host-chosen directory and the
//     repository was left unreadable ("could not parse HEAD").
//     GIT_ALTERNATE_OBJECT_DIRECTORIES — a blob from a donor repository resolved
//     INSIDE the clone: content the lockfile never named, readable from a selected
//     repo, which is the same reach as GHORG_INCLUDE_SUBMODULES by another route.
//     GIT_NAMESPACE — the push landed on the remote at
//     refs/namespaces/<ns>/refs/heads/<branch> instead of the branch, writing a ref
//     into a fleet repo at a path nobody asked for.
//
//     Scrubbing this family cannot break a goldfinger run, and that is exactly what
//     separates it from the config-file pointers excluded below: no delegate reads
//     any of them, both rely on directory discovery, and a run in which one is set
//     is already doing something other than what it announced.
//
//   - The SUBSTITUTION family changes WHAT the clone contains without changing
//     where it came from, so the run succeeds and the resulting PR looks ordinary.
//     GIT_REPLACE_REF_BASE points git's replace mechanism at a ref namespace the
//     host controls: verified, a file whose committed content was REAL was served
//     as FAKE to every read, so a change command greps, edits and commits against
//     content the selected repository does not hold. GIT_SHALLOW_FILE and
//     GIT_GRAFT_FILE do the same to ancestry — verified, a three-commit history
//     presented as two — and GIT_SHALLOW_FILE additionally collides with a flag
//     goldfinger really passes: --clone-depth makes a shallow clone, whose boundary
//     git records in .git/shallow, so an inherited pointer writes that state to a
//     host file and reads the host's back. Neither delegate sets any of the three,
//     git defaults each to a path inside the repository, and no goldfinger
//     operation wants a history other than the one the remote actually has.
//
// A trailing "*" is a prefix match (see overrideEnv). It is used for exactly the
// two families git defines open-endedly — GIT_TRACE2 and GIT_TRACE2_EVENT postdate
// GIT_TRACE, and GIT_CONFIG_KEY_n is unbounded by construction — so those two would
// otherwise need re-auditing against every git release, which is the fragility this
// scrub exists to remove. Every other entry is spelled out, because a prefix that
// reached further would swallow the deliberate exclusions below. GIT_CURL_VERBOSE
// is named separately because it is the one tracing knob that predates the prefix.
//
// DELIBERATELY NOT HERE. Each group below was reached by the same rule, so the
// exclusions are as much a part of the classification as the entries — and each is
// bounded rather than lazy:
//
//   - The IDENTITY variables GIT_AUTHOR_NAME/EMAIL/DATE and
//     GIT_COMMITTER_NAME/EMAIL/DATE. These are the closest call in the whole
//     classification, because they really do outrank config: verified, a commit
//     made in multi-gitter's shape was authored by an ambient
//     "Ambient <ambient@attacker.example>" while the operator's own user.name sat
//     unused in ~/.gitconfig. Two things keep them out. First, goldfinger already
//     RESOLVES AND ANNOUNCES them — loadGitConfig folds them into the identity
//     doctor reports (cmd/gitconfig.go, applyEnvIdentity) — so invariant (a) is
//     satisfied here by disclosure rather than by removal, which is the weaker
//     mechanism but the correct one for a value the operator is entitled to set.
//     Second, removal is not monotonically safer: an environment whose identity
//     comes ONLY from these variables is the ordinary CI shape, and scrubbing them
//     there does not restore some truer identity, it leaves git with none and the
//     commit fails outright. multi-gitter reads them the same way — it sets these
//     exact four itself when given --author-name/--author-email
//     (internal/git/cmdgit/git.go:96-101), flags goldfinger deliberately never
//     passes (see signArgs). The dates are excluded with the names: a backdated
//     commit is cosmetic, and the same CI environments set them on purpose.
//
//   - GIT_CONFIG_GLOBAL, GIT_CONFIG_SYSTEM, GIT_CONFIG_NOSYSTEM. These point git
//     at a different config file, so they can carry the same insteadOf attack as
//     the injection triple above. The obvious move is to scrub them at least on
//     the mirror side, where nothing signs and the local-signing reason below
//     cannot apply. Do not: for this family, removing the variable does not remove
//     the attack, it MOVES it. With GIT_CONFIG_GLOBAL gone git falls back to
//     ~/.gitconfig, which can hold the identical url.<host>.insteadOf rule — and
//     the dominant legitimate use of these variables is HARDENING, since
//     GIT_CONFIG_GLOBAL=/dev/null is how a caller says "ignore my global config".
//     So the scrub inverts. Verified with a hostile ~/.gitconfig in place:
//     GIT_CONFIG_GLOBAL=/dev/null BLOCKS the redirect, and deleting the variable
//     ACTIVATES it. This is the one candidate whose removal is not monotonically
//     safer, which is precisely why it is not an entry. Closing the file route
//     means PINNING an empty global and system config rather than deleting the
//     pointer, and that costs the operator's http.proxy and http.sslCAInfo on
//     every clone — its own trade with its own threat model, not a line appended
//     to this list.
//
//     On the apply side there is additionally nothing to split by sign mode. git
//     only runs as a grandchild under --git-type=cmd, which signArgs emits for
//     SignLocal alone: SignGitHub takes --api-push and SignNone takes
//     multi-gitter's go-git default, and neither execs git at all. Scrubbing
//     "except when signing locally" would therefore protect the two modes that
//     have no git process and disarm the one that does — --sign=local signs by
//     letting git read the operator's own commit.gpgsign and user.signingkey, and
//     an operator who keeps that config somewhere non-default says so with
//     GIT_CONFIG_GLOBAL. Scrubbing it there would silently produce UNSIGNED commits
//     on a run that announced signing, trading a redirect that needs a hostile
//     environment for a broken guarantee that needs only an unusual one. Closing
//     that properly means verifying the signature after the fact.
//
//   - The other HARDENING switches, excluded for that same monotonicity reason:
//     GIT_NO_REPLACE_OBJECTS, GIT_ATTR_NOSYSTEM, GIT_ALLOW_PROTOCOL,
//     GIT_PROTOCOL_FROM_USER and GIT_TERMINAL_PROMPT. Each can only make a run
//     STRICTER than git's default, so scrubbing one takes protection away and adds
//     none. GIT_NO_REPLACE_OBJECTS is the neatest case: it disables the very
//     mechanism GIT_REPLACE_REF_BASE abuses, so it is the counterpart of an entry
//     above rather than a peer of it. GIT_TERMINAL_PROMPT=0 is what turns a bad
//     credential into a fast failure instead of a delegate blocked on a prompt
//     nobody can see.
//
//   - The ones that are simply INERT for these two callers. Each was tested rather
//     than reasoned about, and a scrub that changes nothing is noise in a list whose
//     value is that every entry means something. GIT_CEILING_DIRECTORIES and
//     GIT_DISCOVERY_ACROSS_FILESYSTEM only limit how far UP git may search, and both
//     delegates run git with the working directory already AT the repository root,
//     so the .git it needs is the first one tried — they can make discovery fail
//     loudly, never succeed at a different repository. GIT_COMMON_DIR is only
//     consulted alongside GIT_DIR, which is scrubbed; set alone, refs still resolved
//     from the real repository. GIT_PREFIX (git sets it for its own aliases) left
//     the staged set unchanged. The legacy GIT_CONFIG is read only by `git config`,
//     which neither delegate ever runs. GIT_ATTR_GLOBAL left checkout bytes
//     unchanged. GIT_TRANSPORT_HELPER_DEBUG output carried no token. And the
//     pathspec-magic switches GIT_LITERAL_PATHSPECS, GIT_GLOB_PATHSPECS,
//     GIT_NOGLOB_PATHSPECS and GIT_ICASE_PATHSPECS cannot matter, because the only
//     pathspec either delegate passes is ".".
//
//   - git's exec knobs (GIT_SSH_COMMAND, GIT_ASKPASS, GIT_PROXY_COMMAND,
//     GIT_EXTERNAL_DIFF and friends). These run arbitrary commands, which is a
//     strictly worse outcome — but it is also a different claim. goldfinger's
//     invariants are about WHICH IDENTITY acts and WHICH REPOS it acts on; it has
//     never claimed to sandbox delegates it deliberately hands the operator's
//     environment to, and an environment that can set GIT_EXTERNAL_DIFF can set
//     PATH. Scrubbing two of them would imply a containment guarantee the rest of
//     the design does not make. If that guarantee is wanted, it is its own piece of
//     work with its own threat model, not a line appended to this list.
//
//   - Names that are OUTPUTS rather than inputs — GIT_EXEC_PATH, GIT_PROTOCOL,
//     GIT_QUARANTINE_PATH, GIT_REFLOG_ACTION, GIT_AUTHOR_IDENT, GIT_COMMITTER_IDENT
//     and the rest git exports to its own hooks and subprocesses. An inherited one
//     is overwritten by git in the process where it means anything, so scrubbing it
//     changes nothing.
//
//   - The GIT_TEST_* family (about 40 names) and the Windows-only
//     GIT_REDIRECT_STDIN/STDOUT/STDERR. The first is read by git's own test
//     harness, which is not what a goldfinger run executes; the second has no
//     effect on the platforms release.yml builds (linux and darwin only).
var gitGrandchildEnvVars = []string{
	// Disclosure: git writes the token-bearing argv somewhere the host chose.
	"GIT_TRACE*",
	"GIT_CURL_VERBOSE",
	// Redirect: git sends the token-bearing remote to a host goldfinger never
	// resolved, or stops checking who it is talking to.
	"GIT_CONFIG_COUNT",
	"GIT_CONFIG_KEY_*",
	"GIT_CONFIG_VALUE_*",
	"GIT_CONFIG_PARAMETERS",
	"GIT_SSL_NO_VERIFY",
	// Retarget: git acts on a repository, work tree, index or object store other
	// than the clone the delegate chose by setting cmd.Dir.
	"GIT_DIR",
	"GIT_WORK_TREE",
	"GIT_INDEX_FILE",
	"GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_NAMESPACE",
	// Substitute: git serves content or ancestry the selected repository does not
	// have, so the change command reads and edits something else.
	"GIT_REPLACE_REF_BASE",
	"GIT_SHALLOW_FILE",
	"GIT_GRAFT_FILE",
}

// GitGrandchildEnvVars returns a fresh copy of the git variables both delegate
// environments must strip, because each delegate execs git with the resolved PAT
// in argv and without resetting the environment. A copy, for the same reason
// CredentialEnvVars returns one.
func GitGrandchildEnvVars() []string {
	out := make([]string, len(gitGrandchildEnvVars))
	copy(out, gitGrandchildEnvVars)
	return out
}

// Owner types as reported by the GitHub API and stored in a Selection.
const (
	OwnerUser         = "User"
	OwnerOrganization = "Organization"
)

// SelectionFilter records how a selection was resolved, for provenance.
type SelectionFilter struct {
	AllRepos bool     `json:"allRepos"`
	Topics   []string `json:"topics,omitempty"`

	// Repos, when non-empty, marks an EXPLICIT selection: the operator named an
	// exact set of repo basenames (`select --repo` / `--repos-from`) rather than
	// resolving a topic/all-repos filter. It is the explicit-mode marker `check`
	// keys on — for such a selection, drift is the frozen set diffed against live
	// existence, not a re-run of a filter (which would match nothing and report
	// every repo as removed). Additive and omitempty, so an older reader ignores
	// it and a filtered selection omits it entirely.
	Repos []string `json:"repos,omitempty"`
}

// SelectionVersion is the current lockfile schema version. v2 added per-repo
// branch-presence facts (Repo.BranchPresence) and the list of branch names
// checked at selection time (Selection.BranchesChecked); v1 lockfiles carry
// neither and read back with "unknown" branch facts.
const SelectionVersion = 2

// Signing modes for a real apply run. There is no default: a real run must
// state its signing intent explicitly, because commit provenance is not a safe
// thing to leave implicit for an outward-facing, hard-to-reverse action.
const (
	// SignLocal maps to multi-gitter --git-type=cmd: the real git binary runs the
	// commit, so the operator's ~/.gitconfig (commit.gpgsign / user.signingkey)
	// applies and commits are signed with their own GPG key.
	SignLocal = "local"
	// SignGitHub maps to multi-gitter --api-push: commits go through the GitHub
	// API and are signed by GitHub's web-flow key (always "Verified").
	SignGitHub = "github"
	// SignNone applies no signing flag: multi-gitter's default go-git path, which
	// produces unsigned commits.
	SignNone = "none"
)

// validSignModes is the canonical set of accepted signing modes and the single
// source of truth: cmd's --sign validator and the guide --json catalogue (via
// SignModes) and apply.Apply's execution-boundary guard (via IsValidSignMode)
// all derive from it, so no layer can drift from another. It is unexported so no
// other package can mutate the list the safety guard trusts — an exported slice
// could be appended to (e.g. add "") to defeat the check. There is deliberately
// no default: a run must name a mode.
var validSignModes = []string{SignLocal, SignGitHub, SignNone}

// SignModes returns a fresh copy of the accepted signing modes, for callers that
// need to enumerate them (the CLI validator and the guide --json catalogue). A
// copy, so a caller holding the result cannot mutate the canonical list.
func SignModes() []string {
	out := make([]string, len(validSignModes))
	copy(out, validSignModes)
	return out
}

// IsValidSignMode reports whether mode is a recognised signing mode. It reads the
// unexported canonical list, so its verdict cannot be altered by another package.
func IsValidSignMode(mode string) bool {
	for _, m := range validSignModes {
		if mode == m {
			return true
		}
	}
	return false
}

// ApplySpec is the change to run across a selection via multi-gitter. It is
// assembled in cmd/ from flags.
type ApplySpec struct {
	Branch        string
	BaseBranch    string // base for the PR (e.g. "main" or "dev"); empty = repo default
	CommitMessage string
	PRTitle       string
	PRBody        string
	Labels        []string
	Reviewers     []string
	Draft         bool
	DryRun        bool

	// Confirm authorizes a live (non-dry-run) apply that opens PRs. apply.Apply
	// refuses a run with DryRun=false && Confirm=false, so the charter invariant
	// "a real run needs an explicit confirmation" holds at the execution boundary
	// even for a caller that bypasses the Cobra --confirm flag (e.g. a future MCP
	// adapter), not just in cmd/.
	Confirm bool

	Script []string // the command to run in each repo, e.g. ["sed", "-i", ...]

	// Sign selects how commits are signed: SignLocal (the operator's own GPG key
	// via the git binary), SignGitHub (GitHub's web-flow key via the API), or
	// SignNone (unsigned). There is no default — a real run must set it.
	Sign string

	// BatchSize and BatchPause throttle PR creation to stay under GitHub's
	// secondary rate limits (80 content-generating requests/min). When BatchSize
	// > 0, apply runs multi-gitter over the selection in chunks of that many
	// repos, sleeping BatchPause between chunks. Zero BatchSize = one run over the
	// whole selection (no throttling). Note: neither beats GitHub's 500
	// content-request/hour ceiling — a large fleet must spread across hours, which
	// re-running (multi-gitter skips repos already done) does naturally.
	BatchSize  int
	BatchPause time.Duration
}

// Selection is the frozen set of repos a run targets: the shared artifact that
// both `mirror` (ghorg) and `apply` (multi-gitter) consume, so they operate on a
// provably identical set.
type Selection struct {
	Version    int             `json:"version"`
	Owner      string          `json:"owner"`
	OwnerType  string          `json:"ownerType"` // "User" | "Organization"
	Filter     SelectionFilter `json:"filter"`
	ResolvedAt time.Time       `json:"resolvedAt"`
	Tool       string          `json:"tool"`
	Repos      []Repo          `json:"repos"`

	// BranchesChecked lists the branch names whose presence was recorded at
	// selection time (via `select --branch-presence`). A repo's BranchPresence
	// map holds one entry per name here; a name equal to the repo's own default
	// branch is recorded as present without an API call.
	BranchesChecked []string `json:"branchesChecked,omitempty"`
}
