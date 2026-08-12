package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/redscaresu/goldfinger/client"
	"github.com/redscaresu/goldfinger/mirror"
	"github.com/redscaresu/goldfinger/models"
	"github.com/redscaresu/goldfinger/selection"
	"github.com/spf13/cobra"
)

// mcpServerName / mcpServerTitle identify goldfinger to an MCP client in the
// initialize handshake.
const (
	mcpServerName  = "goldfinger"
	mcpServerTitle = "goldfinger"
)

// mcpOutputTailLimit caps how much delegate output a tool result carries back to
// an MCP caller. mcpRun already bounds the capture to 1 MiB; this trims it far
// further for the JSON-RPC response — an agent wants the final status/error tail,
// not the whole clone log (which the mirror workspace and any log file still
// hold). Redaction happens upstream in mcpDelegate, so this only ever slices
// already-safe bytes.
const mcpOutputTailLimit = 8 << 10 // 8 KiB

func newMCPCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Serve goldfinger's read-and-plan surface to an AI agent over MCP (stdio)",
		Long: "mcp runs goldfinger as a Model Context Protocol server over stdio, exposing " +
			"its read-only and plan-only surface as MCP tools: guide/schema/selections " +
			"(catalogue and contracts), check/select/mirror (discovery, freezing, and " +
			"local mirroring), workspaces_list, doctor, and apply_plan.\n\n" +
			"apply is deliberately NOT a tool. A real apply opens PRs and is the human's " +
			"to run: apply_plan instead returns the exact, digest-bound `goldfinger apply` " +
			"command (dry-run and live variants) for a human to review and execute. The " +
			"server never opens PRs and never runs git.\n\n" +
			"stdin/stdout are the JSON-RPC channel — do not pipe anything else into them. " +
			"The server runs until stdin closes.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMCPServer(cmd.Context())
		},
	}
}

// runMCPServer builds the server and serves it over stdio until the client
// disconnects (stdin EOF) or the context is cancelled. The StdioTransport owns
// the process's real stdin/stdout as the JSON-RPC channel, so nothing here may
// write to them — every tool's delegate output is captured, never streamed.
func runMCPServer(ctx context.Context) error {
	return newMCPServer().Run(ctx, &mcp.StdioTransport{})
}

// mcpDeps holds the environment-touching collaborators the network tools reach
// through, so a test can drive check/mirror hermetically — no token, no GitHub,
// no ghorg on PATH. defaultMCPDeps wires the real implementations; a test swaps
// in fakes. Only the tools that leave the closed world take deps: the offline
// catalogue/contract handlers stay bare funcs.
type mcpDeps struct {
	// resolveToken returns the token and its source (env var name / keychain).
	resolveToken func(context.Context) (string, string, error)
	// listRepos performs live discovery for a selection's owner, returning the
	// raw repos and the owner's live type (user/org).
	listRepos func(context.Context, string) ([]models.Repo, string, error)
	// requireTool asserts a child CLI (e.g. ghorg) is on PATH.
	requireTool func(name, installHint string) error
	// verifyLogin resolves the GitHub principal a token authenticates as, failing
	// fast on a bad token before a long clone.
	verifyLogin func(context.Context, string) (string, error)
	// delegate runs a child tool with output captured and token-redacted — never
	// touching the server's real stdio.
	delegate func(ctx context.Context, name string, args, env []string, token string) ([]byte, error)
}

// defaultMCPDeps wires the real collaborators used in production. The default
// listRepos resolves a token and builds a client per call (mcpClient), matching
// the standalone check command.
func defaultMCPDeps() mcpDeps {
	return mcpDeps{
		resolveToken: resolveToken,
		listRepos: func(ctx context.Context, owner string) ([]models.Repo, string, error) {
			c, err := mcpClient(ctx)
			if err != nil {
				return nil, "", err
			}
			return c.ListRepos(ctx, owner)
		},
		requireTool: requireTool,
		verifyLogin: verifyLoginWithClient,
		delegate:    mcpDelegate,
	}
}

// newMCPServer constructs the server with the production collaborators.
func newMCPServer() *mcp.Server {
	return newMCPServerWithDeps(defaultMCPDeps())
}

// newMCPServerWithDeps constructs the server with every tool registered, reaching
// the environment through d. It is separated from Run so tests can inspect the
// registered tool set (and prove `apply` is not among them) without opening a
// transport, and drive the network tools through fake deps.
func newMCPServerWithDeps(d mcpDeps) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    mcpServerName,
		Title:   mcpServerTitle,
		Version: version,
		Description: "Resolve a repo selection, freeze it, mirror it, and plan a fleet change. " +
			"Read-and-plan only: it never opens PRs and never runs git.",
	}, nil)

	// Read-only, offline catalogue/contract tools.
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "guide",
		Description: "Return goldfinger's machine-readable capabilities catalogue (the same document as `guide --json`): every command, its flags, required flags, enum values, and a canonical example. Start here to learn the surface.",
		Annotations: readOnlyLocal("goldfinger guide"),
	}, mcpGuideHandler)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "schema",
		Description: "Return the JSON Schema for the selection lockfile and every machine-readable payload (the same document as `goldfinger schema`). Use it to validate any goldfinger output offline.",
		Annotations: readOnlyLocal("goldfinger schema"),
	}, mcpSchemaHandler)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "selections",
		Description: "List the named selections in the registry with their owner, repo count, digest, and resolved time (the same document as `selections --json`).",
		Annotations: readOnlyLocal("goldfinger selections"),
	}, mcpSelectionsHandler)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "workspaces_list",
		Description: "List the ephemeral mirror snapshot workspaces under the workspace root (the same document as `workspaces list --json`). Read-only: it never prunes.",
		Annotations: readOnlyLocal("goldfinger workspaces list"),
	}, mcpWorkspacesListHandler)

	// Read-only tools that reach GitHub / the environment.
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "doctor",
		Description: "Run read-only preflight checks: which token source and GitHub principal a run would use, whether ghorg and multi-gitter are on PATH, and whether a git identity and signing are configured. Never writes to GitHub, never runs git, never prints the token.",
		Annotations: readOnlyRemote("goldfinger doctor"),
	}, mcpDoctorHandler)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "check",
		Description: "Report whether a selection has drifted from live discovery: repos added/removed, default branches moved, owner type flipped. Read-only — it never rewrites the lockfile.",
		Annotations: readOnlyRemote("goldfinger check"),
	}, d.mcpCheckHandler)

	// Tools with allowed local side-effects (writing a lockfile, cloning). None
	// writes to GitHub.
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "select",
		Description: "Resolve an owner's repos by topic, all repos, or an explicit set of named repos, and freeze them as a selection lockfile. Writes the lockfile locally; it never writes to GitHub. Returns the written path, the full lockfile, and its repo-set digest.",
		Annotations: writeLocalRemote("goldfinger select"),
	}, mcpSelectHandler)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "mirror",
		Description: "Clone a frozen selection into a local workspace via ghorg. Local side-effects only (clones on disk); it never writes to GitHub and never runs git itself. Returns the workspace path and a coverage report.",
		Annotations: writeLocalRemote("goldfinger mirror --purpose <name>"),
	}, d.mcpMirrorHandler)

	// Plan-only: NEVER opens PRs. Returns the digest-bound apply command for a
	// human to review and run.
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "apply_plan",
		Description: "Plan a fleet change WITHOUT running it. Returns the invocation plan (what would run, not a diff) and the exact, digest-bound `goldfinger apply` command — a dry-run variant and a live variant — for a human to review and execute. This tool never opens PRs, never runs multi-gitter, and needs no token: opening PRs is the human's to do.",
		Annotations: readOnlyLocal("goldfinger apply_plan"),
	}, mcpApplyPlanHandler)

	return srv
}

// --- annotation helpers ----------------------------------------------------

func ptrBool(b bool) *bool { return &b }

// readOnlyLocal marks a tool that reads only and touches neither GitHub nor any
// other network (a closed world).
func readOnlyLocal(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptrBool(false), Title: title}
}

// readOnlyRemote marks a tool that reads only but reaches GitHub (an open world),
// so a repeated call can observe a changed remote.
func readOnlyRemote(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptrBool(true), Title: title}
}

// writeLocalRemote marks a tool with local side-effects that also reaches GitHub
// (read-only, on the GitHub side): select and mirror. It is neither read-only nor
// safely re-runnable-without-effect — select overwrites the lockfile, and mirror
// runs ghorg with cleaning on by default, which can discard local changes in an
// existing clone. So it is marked destructive and non-idempotent, and a host must
// not silently auto-run it: an operator/human should confirm.
func writeLocalRemote(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: ptrBool(true), IdempotentHint: false, OpenWorldHint: ptrBool(true), Title: title}
}

// --- shared input fields ---------------------------------------------------

// mcpSelectionRef names a selection the way the CLI's --name/--selection flags
// do: exactly one, or neither for the default lockfile. Embedded by the tools
// that read an existing lockfile.
type mcpSelectionRef struct {
	Name string `json:"name,omitempty" jsonschema:"named selection in the registry (mutually exclusive with path)"`
	Path string `json:"path,omitempty" jsonschema:"explicit path to the selection lockfile (mutually exclusive with name; default ./goldfinger.selection)"`
}

// --- guide / schema / selections / workspaces_list -------------------------

func mcpGuideHandler(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, capabilities, error) {
	return nil, buildCapabilities(newRootCmd()), nil
}

func mcpSchemaHandler(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, schemaCatalogue, error) {
	return nil, buildSchemaCatalogue(), nil
}

func mcpSelectionsHandler(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, selectionsReport, error) {
	names, err := selection.Names()
	if err != nil {
		return nil, selectionsReport{}, err
	}
	return nil, buildSelectionsReport(names), nil
}

func mcpWorkspacesListHandler(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, workspacesReport, error) {
	root, err := resolveWorkspaceRoot("")
	if err != nil {
		return nil, workspacesReport{}, err
	}
	all, err := scanWorkspaces(root)
	if err != nil {
		return nil, workspacesReport{}, err
	}
	return nil, workspacesReport{
		Version:    workspacesReportVersion,
		Root:       root,
		Action:     workspaceActionList,
		Pruned:     false,
		Workspaces: nonNilWorkspaces(all),
	}, nil
}

// --- doctor ----------------------------------------------------------------

func mcpDoctorHandler(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, doctorReport, error) {
	deps := doctorDeps{
		resolveToken: resolveToken,
		verifyLogin:  verifyLoginWithClient,
		probeTool:    probeToolDefault,
		loadConfig:   loadGitConfig,
	}
	checks := gatherDoctorChecks(ctx, deps)
	return nil, doctorReport{Version: doctorReportVersion, Checks: checks}, nil
}

// --- check -----------------------------------------------------------------

func (d mcpDeps) mcpCheckHandler(ctx context.Context, _ *mcp.CallToolRequest, in mcpSelectionRef) (*mcp.CallToolResult, checkReport, error) {
	path, err := resolveSelectionPath(in.Name, in.Path)
	if err != nil {
		return nil, checkReport{}, err
	}
	sel, err := selection.Read(path)
	if err != nil {
		return nil, checkReport{}, err
	}
	raw, liveOwnerType, err := d.listRepos(ctx, sel.Owner)
	if err != nil {
		return nil, checkReport{}, err
	}
	res := computeCheckResult(sel, raw, liveOwnerType)
	// Drift is a successful result (InSync:false), not an error — the exit-code
	// convention is a CLI nicety, meaningless over MCP.
	return nil, buildCheckReport(in.Name, res), nil
}

// --- select ----------------------------------------------------------------

type mcpSelectIn struct {
	mcpSelectionRef
	Org            string   `json:"org" jsonschema:"the GitHub owner (org or user login) to resolve repos for"`
	AllRepos       bool     `json:"all_repos,omitempty" jsonschema:"select every non-archived repo (one selection mode; mutually exclusive with topics/repos)"`
	Topics         []string `json:"topics,omitempty" jsonschema:"select repos carrying any of these topics (one selection mode; mutually exclusive with all_repos/repos)"`
	Repos          []string `json:"repos,omitempty" jsonschema:"select this explicit set of repo basenames under org (one selection mode; mutually exclusive with all_repos/topics); a named repo that 404s is a hard error and archived repos are included"`
	BranchPresence []string `json:"branch_presence,omitempty" jsonschema:"record, read-only, whether each selected repo has these branches, frozen into the lockfile for a later mirror --branch"`
	AllowEmpty     bool     `json:"allow_empty,omitempty" jsonschema:"write a lockfile even when the selection matches zero repos (default: a zero-repo result is an error)"`
}

func mcpSelectHandler(ctx context.Context, _ *mcp.CallToolRequest, in mcpSelectIn) (*mcp.CallToolResult, selectJSONReport, error) {
	t, err := prepareTargeting(targeting{org: in.Org, allRepos: in.AllRepos, topics: in.Topics, repos: in.Repos})
	if err != nil {
		return nil, selectJSONReport{}, err
	}
	path, err := resolveSelectionPath(in.Name, in.Path)
	if err != nil {
		return nil, selectJSONReport{}, err
	}
	token, source, err := resolveToken(ctx)
	if err != nil {
		return nil, selectJSONReport{}, err
	}
	c, err := client.New(token)
	if err != nil {
		return nil, selectJSONReport{}, err
	}
	// Verify resolves the principal used only to make an empty-selection
	// diagnostic actionable; a bad token also fails fast here.
	login, err := c.Verify(ctx)
	if err != nil {
		return nil, selectJSONReport{}, fmt.Errorf("verifying token: %w", err)
	}
	o := selectOpts{
		t:               t,
		branchesToCheck: in.BranchPresence,
		selectionPath:   path,
		tool:            "goldfinger " + version,
		source:          source,
		allowEmpty:      in.AllowEmpty,
	}
	// io.Discard: the branch-presence banner is a human aid the MCP caller does
	// not consume (stdout/stderr are off-limits from inside the server anyway).
	sel, err := buildSelectionFromLive(ctx, c, o, login, io.Discard)
	if err != nil {
		return nil, selectJSONReport{}, err
	}
	if err := selection.Write(path, sel, selection.WriteOptions{Overwrite: true}); err != nil {
		return nil, selectJSONReport{}, err
	}
	_, digest := selection.Digest(sel)
	return nil, selectJSONReport{SelectionPath: path, Selection: sel, Digest: digest}, nil
}

// --- mirror ----------------------------------------------------------------

type mcpMirrorIn struct {
	mcpSelectionRef
	Workspace   string `json:"workspace,omitempty" jsonschema:"absolute workspace dir (default ~/goldfinger); mutually exclusive with purpose"`
	Purpose     string `json:"purpose,omitempty" jsonschema:"ephemeral, timestamped workspace ~/goldfinger/<purpose>-<stamp>; mutually exclusive with workspace"`
	Branch      string `json:"branch,omitempty" jsonschema:"checkout this branch in every cloned repo (cannot be combined with clone_depth > 0)"`
	Concurrency int    `json:"concurrency,omitempty" jsonschema:"concurrent clones (0 = ghorg default)"`
	CloneDepth  int    `json:"clone_depth,omitempty" jsonschema:"shallow clone depth (0 = full history); incompatible with branch"`
	NoClean     bool   `json:"no_clean,omitempty" jsonschema:"preserve local changes in existing clones (skip ghorg's git clean on re-sync)"`
	DryRun      bool   `json:"dry_run,omitempty" jsonschema:"show what ghorg would clone without cloning"`
}

// mcpMirrorResult is the mirror tool's typed output. On a successful non-dry-run
// mirror, Report carries the same coverage summary as `mirror --report-json`;
// on a dry-run (which clones nothing) Report is null. OutputTail is the tail of
// ghorg's captured output with the token already redacted.
type mcpMirrorResult struct {
	Workspace  string        `json:"workspace"`
	Owner      string        `json:"owner"`
	RepoCount  int           `json:"repoCount"`
	Branch     string        `json:"branch,omitempty"`
	DryRun     bool          `json:"dryRun"`
	Report     *mirrorReport `json:"report"`
	OutputTail string        `json:"outputTail"`
}

func (d mcpDeps) mcpMirrorHandler(ctx context.Context, _ *mcp.CallToolRequest, in mcpMirrorIn) (*mcp.CallToolResult, mcpMirrorResult, error) {
	if err := validateMirror(mirrorValidation{branch: in.Branch, cloneDepth: in.CloneDepth}); err != nil {
		return nil, mcpMirrorResult{}, err
	}
	path, err := resolveSelectionPath(in.Name, in.Path)
	if err != nil {
		return nil, mcpMirrorResult{}, err
	}
	sel, err := selection.Read(path)
	if err != nil {
		return nil, mcpMirrorResult{}, err
	}
	if len(sel.Repos) == 0 {
		return nil, mcpMirrorResult{}, fmt.Errorf("selection is empty — nothing to mirror; re-run select (a 0-repo select is an error unless allow_empty)")
	}
	// Caveat: a --purpose workspace is stamped to the millisecond (resolveWorkspace),
	// so two concurrent mirror calls with the same purpose+branch resolving in the
	// same millisecond would target one dir and interleave. That window is ~1 ms and
	// needs two identical-purpose mirrors at once (an operator mistake, not routine
	// automation); the fix would change the shared workspace-naming contract, so it
	// is out of scope here. Use distinct --purpose names for concurrent mirrors.
	ws, snap, err := resolveWorkspace(in.Workspace, in.Purpose, in.Branch)
	if err != nil {
		return nil, mcpMirrorResult{}, err
	}
	token, _, err := d.resolveToken(ctx)
	if err != nil {
		return nil, mcpMirrorResult{}, err
	}
	if err := d.requireTool("ghorg", "https://github.com/gabrie30/ghorg#installation"); err != nil {
		return nil, mcpMirrorResult{}, err
	}
	// Fail fast on a bad token before the (potentially long) clone, honouring the
	// verify-identity-first posture without re-running discovery.
	if _, err := d.verifyLogin(ctx, token); err != nil {
		return nil, mcpMirrorResult{}, fmt.Errorf("verifying token: %w", err)
	}

	opts := mirror.Options{
		Workspace:   ws,
		Branch:      in.Branch,
		Concurrency: in.Concurrency,
		CloneDepth:  in.CloneDepth,
		NoClean:     in.NoClean,
		DryRun:      in.DryRun,
	}
	// The stdio-safe runner captures ghorg's output (never touching the server's
	// stdio) and redacts the token from it (mcpDelegate). mirror.Mirror injects
	// the token into the child env, so it never reaches argv.
	var captured []byte
	run := func(ctx context.Context, name string, args, env []string) error {
		out, rerr := d.delegate(ctx, name, args, env, token)
		captured = out
		return rerr
	}
	if err := mirror.Mirror(ctx, run, sel, token, opts); err != nil {
		return nil, mcpMirrorResult{}, fmt.Errorf("ghorg clone %s failed: %w (output tail: %s)", sel.Owner, err, mcpTail(captured))
	}

	res := mcpMirrorResult{
		Workspace:  ws,
		Owner:      sel.Owner,
		RepoCount:  len(sel.Repos),
		Branch:     in.Branch,
		DryRun:     in.DryRun,
		OutputTail: mcpTail(captured),
	}
	// A dry-run clones nothing, so an on-disk reconciliation would be misleading —
	// mirror the CLI and skip the report (and the snapshot manifest) there.
	if !in.DryRun {
		rec := reconcile(sel, ws, opts)
		rep := buildMirrorReport(sel, ws, opts, rec)
		res.Report = &rep
		if snap != nil {
			snap.Owner = sel.Owner
		}
		if err := writeSnapshotManifest(ws, snap, in.DryRun); err != nil {
			return nil, mcpMirrorResult{}, err
		}
	}
	return nil, res, nil
}

// mcpTail returns the last mcpOutputTailLimit bytes of b as a string. b is
// already token-redacted by mcpDelegate before it reaches here.
func mcpTail(b []byte) string {
	if len(b) > mcpOutputTailLimit {
		b = b[len(b)-mcpOutputTailLimit:]
	}
	return string(b)
}

// --- apply_plan ------------------------------------------------------------

type mcpApplyPlanIn struct {
	mcpSelectionRef
	Branch        string   `json:"branch" jsonschema:"branch to commit changes to (required)"`
	CommitMessage string   `json:"commit_message" jsonschema:"commit message (required)"`
	PRTitle       string   `json:"pr_title" jsonschema:"pull request title (required)"`
	Sign          string   `json:"sign" jsonschema:"how to sign commits (required): local (your GPG key), github (GitHub-verified), or none (unsigned)"`
	Script        []string `json:"script" jsonschema:"the change command and its args, e.g. [\"sed\",\"-i\",\"s/old/new/\",\"go.mod\"] (required)"`
	BaseBranch    string   `json:"base_branch,omitempty" jsonschema:"base branch for the PR (default: each repo's default branch)"`
	PRBody        string   `json:"pr_body,omitempty" jsonschema:"pull request body"`
	Labels        []string `json:"labels,omitempty" jsonschema:"labels to add to every PR"`
	Reviewers     []string `json:"reviewers,omitempty" jsonschema:"reviewers to request: user or org/team"`
	Draft         bool     `json:"draft,omitempty" jsonschema:"open PRs as drafts"`
	BatchSize     int      `json:"batch_size,omitempty" jsonschema:"open PRs in batches of this many repos (0 = one run over the whole selection)"`
	BatchPause    string   `json:"batch_pause,omitempty" jsonschema:"pause between batches as a Go duration, e.g. 60s (only used with batch_size)"`
}

// mcpCommand is a runnable command in both machine (argv) and human (display)
// form. argv is the exact, full command — argv[0] is "goldfinger", then its args,
// including the operator's own script after "--" — so a client can exec it
// verbatim. display is the same command shell-quoted for a human to read/paste;
// the two never diverge.
type mcpCommand struct {
	Argv    []string `json:"argv"`
	Display string   `json:"display"`
}

// mcpApplyPlanResult is apply_plan's typed output. It carries the invocation plan
// plus the two digest-bound commands a human runs: DryRunCommand previews, and
// LiveCommand (with --dry-run=false --confirm) opens PRs. Both pin the exact
// lockfile via --selection and --expect-selection-sha256, so the command a human
// reviews is provably the selection the plan was built from.
type mcpApplyPlanResult struct {
	Plan            applyPlan  `json:"plan"`
	SelectionPath   string     `json:"selectionPath"`
	SelectionSHA256 string     `json:"selectionSha256"`
	DryRunCommand   mcpCommand `json:"dryRunCommand"`
	LiveCommand     mcpCommand `json:"liveCommand"`
	Notice          string     `json:"notice"`
}

func mcpApplyPlanHandler(_ context.Context, _ *mcp.CallToolRequest, in mcpApplyPlanIn) (*mcp.CallToolResult, mcpApplyPlanResult, error) {
	if err := validateApply(applyValidation{
		branch:        in.Branch,
		commitMessage: in.CommitMessage,
		prTitle:       in.PRTitle,
		sign:          in.Sign,
		script:        in.Script,
	}); err != nil {
		return nil, mcpApplyPlanResult{}, err
	}
	var batchPause time.Duration
	if in.BatchPause != "" {
		d, err := time.ParseDuration(in.BatchPause)
		if err != nil {
			return nil, mcpApplyPlanResult{}, fmt.Errorf("invalid batch_pause: %w", err)
		}
		batchPause = d
	}
	path, err := resolveSelectionPath(in.Name, in.Path)
	if err != nil {
		return nil, mcpApplyPlanResult{}, err
	}
	// An absolute path so the returned command runs correctly wherever a human
	// invokes it, not only from the server's cwd.
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, mcpApplyPlanResult{}, fmt.Errorf("resolve selection path: %w", err)
	}
	// ReadWithDigest gives the exact-bytes sha256 that binds the command to this
	// precise lockfile (apply's --expect-selection-sha256). Reading the file is
	// this tool's only I/O — it never resolves a token and never runs apply.
	sel, digest, err := selection.ReadWithDigest(absPath)
	if err != nil {
		return nil, mcpApplyPlanResult{}, err
	}
	spec := models.ApplySpec{
		Branch:        in.Branch,
		BaseBranch:    in.BaseBranch,
		CommitMessage: in.CommitMessage,
		PRTitle:       in.PRTitle,
		PRBody:        in.PRBody,
		Labels:        in.Labels,
		Reviewers:     in.Reviewers,
		Draft:         in.Draft,
		DryRun:        true,
		Script:        in.Script,
		Sign:          in.Sign,
		BatchSize:     in.BatchSize,
		BatchPause:    batchPause,
	}
	dryArgv := buildApplyArgv(spec, absPath, digest, false)
	liveArgv := buildApplyArgv(spec, absPath, digest, true)
	return nil, mcpApplyPlanResult{
		Plan:            buildApplyPlan(sel, spec),
		SelectionPath:   absPath,
		SelectionSHA256: digest,
		DryRunCommand:   mcpCommand{Argv: dryArgv, Display: mcpDisplay(dryArgv)},
		LiveCommand:     mcpCommand{Argv: liveArgv, Display: mcpDisplay(liveArgv)},
		Notice: "apply_plan does not open PRs. Review the plan, then a human runs dryRunCommand to preview and " +
			"liveCommand to open PRs. Both are bound to this exact lockfile via --expect-selection-sha256; " +
			"if the selection changes, apply refuses to run and the plan must be rebuilt.",
	}, nil
}

// buildApplyArgv assembles the exact, runnable `goldfinger apply ...` argv for a
// plan — argv[0] is the program itself, so a client can exec the argv verbatim
// (it is not args-to-goldfinger). The selection is pinned by absolute --selection
// and --expect-selection-sha256, so the command a human runs is provably the one
// the plan describes. live adds the real-run guards (--dry-run=false --confirm);
// without them the command is a dry-run (apply's default). The operator's script
// goes last after "--", exactly as supplied.
func buildApplyArgv(spec models.ApplySpec, selectionPath, digest string, live bool) []string {
	argv := []string{
		"goldfinger",
		"apply",
		"--selection", selectionPath,
		"--expect-selection-sha256", digest,
		"--sign", spec.Sign,
		"--branch", spec.Branch,
		"--commit-message", spec.CommitMessage,
		"--pr-title", spec.PRTitle,
	}
	if spec.BaseBranch != "" {
		argv = append(argv, "--base-branch", spec.BaseBranch)
	}
	if spec.PRBody != "" {
		argv = append(argv, "--pr-body", spec.PRBody)
	}
	for _, l := range spec.Labels {
		argv = append(argv, "--label", l)
	}
	for _, r := range spec.Reviewers {
		argv = append(argv, "--reviewer", r)
	}
	if spec.Draft {
		argv = append(argv, "--draft")
	}
	if spec.BatchSize > 0 {
		argv = append(argv, "--batch-size", strconv.Itoa(spec.BatchSize))
	}
	if spec.BatchPause > 0 {
		argv = append(argv, "--batch-pause", spec.BatchPause.String())
	}
	if live {
		argv = append(argv, "--dry-run=false", "--confirm")
	}
	argv = append(argv, "--")
	argv = append(argv, spec.Script...)
	return argv
}

// mcpDisplay renders a full argv (program first) as a copy-pasteable shell
// command line, quoting only the tokens that need it. It is a human aid; Argv is
// the exact source of truth and the two stay in lockstep — both are runnable.
func mcpDisplay(argv []string) string {
	parts := make([]string, 0, len(argv))
	for _, a := range argv {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

// shellQuote single-quotes a token that contains anything outside a conservative
// safe set, escaping embedded single quotes. An empty token becomes ”.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == '/', r == '=', r == ':', r == ',', r == '@', r == '+':
		default:
			safe = false
		}
		if !safe {
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// mcpClient resolves a token and builds an API client — the shared preamble for
// the read-only tools that reach GitHub.
func mcpClient(ctx context.Context) (*client.Client, error) {
	token, _, err := resolveToken(ctx)
	if err != nil {
		return nil, err
	}
	return client.New(token)
}
