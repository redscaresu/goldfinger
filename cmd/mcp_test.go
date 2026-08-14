package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/redscaresu/goldfinger/models"
	"github.com/redscaresu/goldfinger/selection"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// connectMCPTestClient wires an in-memory client to newMCPServer() and returns a
// live client session, so tests exercise the tools over the real MCP protocol
// (list/call round-trip), not just the Go handler functions.
func connectMCPTestClient(t *testing.T) *mcp.ClientSession {
	return connectMCPTestClientWithDeps(t, defaultMCPDeps())
}

// connectMCPTestClientWithDeps is connectMCPTestClient with injectable
// collaborators, so a test can drive the network tools (check/mirror) over the
// real MCP protocol while staying hermetic — no token, no GitHub, no ghorg.
func connectMCPTestClientWithDeps(t *testing.T, d mcpDeps) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	serverT, clientT := mcp.NewInMemoryTransports()

	_, err := newMCPServerWithDeps(d).Connect(ctx, serverT, nil)
	require.NoError(t, err, "server connect")

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	require.NoError(t, err, "client connect")
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// TestMCPServerExposesExactlyTheReadAndPlanTools is the charter guard: the server
// advertises exactly the read-and-plan surface and — critically — no `apply`
// tool. Opening PRs is the human's to run; over MCP it is reachable only as the
// digest-bound command apply_plan hands back, never as an executable tool.
func TestMCPServerExposesExactlyTheReadAndPlanTools(t *testing.T) {
	cs := connectMCPTestClient(t)
	res, err := cs.ListTools(context.Background(), nil)
	require.NoError(t, err)

	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	assert.ElementsMatch(t, []string{
		"guide", "schema", "selections", "check", "select", "mirror",
		"scan", "apply_plan", "workspaces_list", "doctor",
	}, names, "the MCP tool set must be exactly the read-and-plan surface")

	assert.NotContains(t, names, "apply",
		"apply must NEVER be an MCP tool — opening PRs is the human's to run")
}

// TestMCPApplyPlanToolIsMarkedReadOnly asserts the apply_plan tool advertises the
// read-only, non-destructive, closed-world hint set — it plans, it does not act.
func TestMCPApplyPlanToolIsMarkedReadOnly(t *testing.T) {
	cs := connectMCPTestClient(t)
	res, err := cs.ListTools(context.Background(), nil)
	require.NoError(t, err)

	var found *mcp.Tool
	for _, tool := range res.Tools {
		if tool.Name == "apply_plan" {
			found = tool
			break
		}
	}
	require.NotNil(t, found, "apply_plan tool must be present")
	require.NotNil(t, found.Annotations)
	assert.True(t, found.Annotations.ReadOnlyHint, "apply_plan must be read-only")
	require.NotNil(t, found.Annotations.OpenWorldHint)
	assert.False(t, *found.Annotations.OpenWorldHint, "apply_plan is offline — a closed world")
}

// TestMCPWriteToolsAreMarkedDestructive asserts the local-side-effect tools do
// not understate their behaviour: select overwrites the lockfile and mirror can
// discard local changes (ghorg clean), so a host must not treat them as read-only
// or safely-idempotent and silently auto-run them.
func TestMCPWriteToolsAreMarkedDestructive(t *testing.T) {
	cs := connectMCPTestClient(t)
	res, err := cs.ListTools(context.Background(), nil)
	require.NoError(t, err)

	byName := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		byName[tool.Name] = tool
	}
	for _, name := range []string{"select", "mirror"} {
		tool := byName[name]
		require.NotNilf(t, tool, "%s tool must be present", name)
		require.NotNilf(t, tool.Annotations, "%s must carry annotations", name)
		assert.Falsef(t, tool.Annotations.ReadOnlyHint, "%s must not claim read-only", name)
		require.NotNilf(t, tool.Annotations.DestructiveHint, "%s must state a destructive hint", name)
		assert.Truef(t, *tool.Annotations.DestructiveHint, "%s must be marked destructive", name)
		assert.Falsef(t, tool.Annotations.IdempotentHint, "%s must not claim idempotent", name)
	}
}

// writeTestSelection writes a minimal two-repo lockfile and returns its path plus
// the exact-bytes digest apply_plan is expected to bind the command to.
func writeTestSelection(t *testing.T) (path, digest string) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "goldfinger.selection")
	sel := models.Selection{
		Version:    models.SelectionVersion,
		Owner:      "acme",
		OwnerType:  models.OwnerOrganization,
		Filter:     models.SelectionFilter{Topics: []string{"platform"}},
		ResolvedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Tool:       "goldfinger test",
		Repos: []models.Repo{
			{Owner: "acme", Name: "alpha", DefaultBranch: "main"},
			{Owner: "acme", Name: "beta", DefaultBranch: "main"},
		},
	}
	require.NoError(t, selection.Write(path, sel, selection.WriteOptions{Overwrite: true}))
	_, digest, err := selection.ReadWithDigest(path)
	require.NoError(t, err)
	return path, digest
}

// TestMCPApplyPlanReturnsDigestBoundCommandsWithoutRunningApply is the core
// safety test. It calls the apply_plan tool with NO token in the environment and
// asserts it still succeeds — proving the tool never resolves a token, never runs
// multi-gitter, and never opens a PR: it is pure planning. It then asserts the two
// returned commands are bound to the exact lockfile via --expect-selection-sha256,
// and that only the live command carries the real-run guards.
func TestMCPApplyPlanReturnsDigestBoundCommandsWithoutRunningApply(t *testing.T) {
	// No token anywhere, and gh disabled — if apply_plan tried to reach GitHub or
	// run apply, it would fail here. It must not: planning is fully offline.
	t.Setenv(tokenEnvVar, "")
	stubGhToken(t, "", false)

	path, digest := writeTestSelection(t)

	cs := connectMCPTestClient(t)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "apply_plan",
		Arguments: map[string]any{
			"path":           path,
			"branch":         "bump-dep",
			"commit_message": "bump dep",
			"pr_title":       "Bump dep",
			"sign":           models.SignLocal,
			"script":         []string{"sed", "-i", "s/old/new/", "go.mod"},
		},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, "apply_plan must succeed offline with no token: %v", res.Content)

	var out mcpApplyPlanResult
	decodeStructured(t, res, &out)

	assert.Equal(t, digest, out.SelectionSHA256, "the plan must bind the exact-bytes digest")
	assert.True(t, filepath.IsAbs(out.SelectionPath), "the command must pin an absolute selection path")

	// The argv is runnable verbatim: program first, then the apply subcommand, and
	// the display line is the same command (not double-prefixed).
	require.GreaterOrEqual(t, len(out.DryRunCommand.Argv), 2)
	assert.Equal(t, "goldfinger", out.DryRunCommand.Argv[0], "argv[0] must be the program, so the argv runs verbatim")
	assert.Equal(t, "apply", out.DryRunCommand.Argv[1])
	assert.Equal(t, "goldfinger", out.LiveCommand.Argv[0])
	assert.True(t, strings.HasPrefix(out.DryRunCommand.Display, "goldfinger apply "),
		"display must match the argv, not double-prefix the program: %q", out.DryRunCommand.Display)

	// Both commands pin the exact lockfile; only the live one opens PRs.
	assert.Contains(t, out.DryRunCommand.Argv, "--expect-selection-sha256")
	assert.Contains(t, out.DryRunCommand.Argv, digest)
	assert.NotContains(t, out.DryRunCommand.Argv, "--dry-run=false",
		"the dry-run command must not carry the real-run guard")
	assert.NotContains(t, out.DryRunCommand.Argv, "--confirm")

	assert.Contains(t, out.LiveCommand.Argv, "--expect-selection-sha256")
	assert.Contains(t, out.LiveCommand.Argv, "--dry-run=false",
		"the live command must carry the real-run guard")
	assert.Contains(t, out.LiveCommand.Argv, "--confirm")

	// The operator's script survives verbatim after the -- separator in both.
	assert.Equal(t, []string{"sed", "-i", "s/old/new/", "go.mod"}, scriptAfterSeparator(out.DryRunCommand.Argv),
		"the dry-run command must carry the script verbatim after --")
	assert.Equal(t, []string{"sed", "-i", "s/old/new/", "go.mod"}, scriptAfterSeparator(out.LiveCommand.Argv),
		"the live command must carry the script verbatim after --")

	// The plan describes the selection it was built from, not a diff.
	assert.Equal(t, 2, out.Plan.ReposTotal)
	assert.True(t, out.Plan.DryRun, "the plan payload defaults to the dry-run posture")
}

// scriptAfterSeparator returns the argv tokens following the first "--".
func scriptAfterSeparator(argv []string) []string {
	for i, a := range argv {
		if a == "--" {
			return argv[i+1:]
		}
	}
	return nil
}

// TestMCPApplyPlanValidatesInputs asserts apply_plan enforces the same required
// inputs as the CLI apply validator — a missing script is rejected, not silently
// planned into an empty command.
func TestMCPApplyPlanValidatesInputs(t *testing.T) {
	path, _ := writeTestSelection(t)
	cs := connectMCPTestClient(t)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "apply_plan",
		Arguments: map[string]any{
			"path":           path,
			"branch":         "b",
			"commit_message": "m",
			"pr_title":       "t",
			"sign":           models.SignLocal,
			// script omitted
		},
	})
	require.NoError(t, err)
	assert.True(t, res.IsError, "apply_plan must reject a plan with no script")
}

// TestMCPReadOnlyToolsRoundTrip proves the offline catalogue tools answer over the
// protocol without a token: guide and schema are pure, so they must succeed with
// no environment set up at all.
func TestMCPReadOnlyToolsRoundTrip(t *testing.T) {
	t.Setenv(tokenEnvVar, "")
	stubGhToken(t, "", false)
	cs := connectMCPTestClient(t)

	for _, name := range []string{"guide", "schema"} {
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name})
		require.NoErrorf(t, err, "%s call", name)
		assert.Falsef(t, res.IsError, "%s must succeed offline: %v", name, res.Content)
	}
}

// TestBuildApplyArgvIncludesEveryOptionalFlag locks the exact command apply_plan
// hands a human: every optional field must map to its flag (in the documented
// order), list fields must repeat one flag per value, and only a live run may carry
// the real-run guard. This is the runnable contract, so it is worth pinning tightly.
func TestBuildApplyArgvIncludesEveryOptionalFlag(t *testing.T) {
	spec := models.ApplySpec{
		Sign:          models.SignLocal,
		Branch:        "bump-dep",
		CommitMessage: "bump dep",
		PRTitle:       "Bump dep",
		BaseBranch:    "dev",
		PRBody:        "body with spaces",
		Labels:        []string{"dependencies", "automated"},
		Reviewers:     []string{"octocat", "acme/platform"},
		Draft:         true,
		BatchSize:     5,
		BatchPause:    30 * time.Second,
		Script:        []string{"sed", "-i", "s/old/new/", "go.mod"},
	}
	argv := buildApplyArgv(spec, "/abs/platform.selection", "deadbeef", true)

	assert.Equal(t, "dev", argValue(t, argv, "--base-branch"))
	assert.Equal(t, "body with spaces", argValue(t, argv, "--pr-body"))
	assert.Equal(t, []string{"dependencies", "automated"}, argValues(argv, "--label"),
		"each label repeats the --label flag")
	assert.Equal(t, []string{"octocat", "acme/platform"}, argValues(argv, "--reviewer"),
		"each reviewer repeats the --reviewer flag")
	assert.Contains(t, argv, "--draft")
	assert.Equal(t, "5", argValue(t, argv, "--batch-size"))
	assert.Equal(t, "30s", argValue(t, argv, "--batch-pause"), "duration is rendered, not the raw nanoseconds")
	assert.Contains(t, argv, "--dry-run=false", "a live argv carries the real-run guard")
	assert.Contains(t, argv, "--confirm")
	assert.Equal(t, []string{"sed", "-i", "s/old/new/", "go.mod"}, scriptAfterSeparator(argv))

	// Omitted optionals must not leak their flags, and a dry-run argv must not carry
	// the real-run guard — the two postures differ only by that pair.
	bare := buildApplyArgv(models.ApplySpec{
		Sign: models.SignNone, Branch: "b", CommitMessage: "m", PRTitle: "t",
		Script: []string{"true"},
	}, "/abs/s.selection", "d", false)
	for _, flag := range []string{"--base-branch", "--pr-body", "--label", "--reviewer", "--draft", "--batch-size", "--batch-pause", "--dry-run=false", "--confirm"} {
		assert.NotContainsf(t, bare, flag, "a bare dry-run argv must not carry %s", flag)
	}
}

// argValue returns the token following the first occurrence of flag, failing if the
// flag is absent — so a typo in the flag name is caught, not silently passed.
func argValue(t *testing.T, argv []string, flag string) string {
	t.Helper()
	for i, a := range argv {
		if a == flag && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	t.Fatalf("flag %q not found in argv %v", flag, argv)
	return ""
}

// argValues returns the token following every occurrence of flag, in order.
func argValues(argv []string, flag string) []string {
	var out []string
	for i, a := range argv {
		if a == flag && i+1 < len(argv) {
			out = append(out, argv[i+1])
		}
	}
	return out
}

// TestMCPWorkspacesListReflectsRootOnDisk proves the workspaces_list tool answers
// over the protocol from the real on-disk snapshot root, returning a well-formed
// list report — offline, no token.
func TestMCPWorkspacesListReflectsRootOnDisk(t *testing.T) {
	t.Setenv(tokenEnvVar, "")
	stubGhToken(t, "", false)
	home := t.TempDir()
	t.Setenv("HOME", home)
	created := time.Date(2026, 8, 5, 10, 11, 12, 0, time.UTC)
	makeSnapshot(t, filepath.Join(home, "goldfinger"), "audit-2026-08-05-101112.131", "audit", "", created, 2048)

	cs := connectMCPTestClient(t)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "workspaces_list"})
	require.NoError(t, err)
	require.False(t, res.IsError, "workspaces_list must succeed offline: %v", res.Content)

	var out workspacesReport
	decodeStructured(t, res, &out)
	assert.Equal(t, workspaceActionList, out.Action, "the list tool must never report a prune action")
	assert.False(t, out.Pruned)
	require.Len(t, out.Workspaces, 1, "the one stamped snapshot on disk must be listed")
	assert.Contains(t, out.Workspaces[0].Path, "audit-2026-08-05-101112.131")
	assert.Equal(t, "audit", out.Workspaces[0].Purpose, "purpose is recovered from the sidecar manifest")
}

// TestMCPScanSearchesTheMirrorOffline proves the scan tool searches the mirrored
// clones of a selection and returns a match report over the protocol with NO
// token and NO network — the read-and-plan charter applied to the read path. It
// also proves the honesty guarantee: a selected repo not on disk is reported
// scanned:false rather than dropped.
func TestMCPScanSearchesTheMirrorOffline(t *testing.T) {
	t.Setenv(tokenEnvVar, "")
	stubGhToken(t, "", false)
	ws := t.TempDir()
	mkClone(t, ws, "acme", "alpha", map[string]string{"Dockerfile": "FROM debian:bullseye\n"})
	// "beta" is in the selection but never mirrored — must surface as not scanned.
	selPath := filepath.Join(t.TempDir(), "goldfinger.selection")
	require.NoError(t, selection.Write(selPath, scanSelection("acme", "alpha", "beta"), selection.WriteOptions{Overwrite: true}))

	cs := connectMCPTestClient(t)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "scan",
		Arguments: map[string]any{"path": selPath, "workspace": ws, "pattern": "debian:bullseye"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, "scan must succeed offline with no token: %v", res.Content)

	var out scanReport
	decodeStructured(t, res, &out)
	assert.Equal(t, scanReportVersion, out.Version)
	assert.Equal(t, 2, out.ReposInSelection)
	assert.Equal(t, 1, out.ReposScanned)
	assert.Equal(t, 1, out.ReposNotScanned, "the unmirrored repo is accounted for, not dropped")
	assert.Equal(t, 1, out.TotalMatches)
	require.Len(t, out.Repos, 2)
	assert.Equal(t, "acme/alpha", out.Repos[0].Repo)
	require.Len(t, out.Repos[0].Matches, 1)
	assert.Equal(t, "Dockerfile", out.Repos[0].Matches[0].Path)
	assert.False(t, out.Repos[1].Scanned, "acme/beta was never mirrored")
	assert.Equal(t, skipReasonNotMirrored, out.Repos[1].SkipReason)
}

// TestMCPScanToolIsReadOnlyOffline asserts the scan tool advertises the read-only,
// closed-world hint set: it reads local files, never GitHub, never git.
func TestMCPScanToolIsReadOnlyOffline(t *testing.T) {
	cs := connectMCPTestClient(t)
	res, err := cs.ListTools(context.Background(), nil)
	require.NoError(t, err)

	var found *mcp.Tool
	for _, tool := range res.Tools {
		if tool.Name == "scan" {
			found = tool
			break
		}
	}
	require.NotNil(t, found, "scan tool must be present")
	require.NotNil(t, found.Annotations)
	assert.True(t, found.Annotations.ReadOnlyHint, "scan must be read-only")
	require.NotNil(t, found.Annotations.OpenWorldHint)
	assert.False(t, *found.Annotations.OpenWorldHint, "scan is offline — a closed world")
}

// TestMCPSelectionsReflectsRegistryOnDisk proves the selections tool enumerates the
// named lockfiles in the registry dir and reports each one's repo count — offline.
func TestMCPSelectionsReflectsRegistryOnDisk(t *testing.T) {
	t.Setenv(tokenEnvVar, "")
	stubGhToken(t, "", false)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	path, err := selection.PathForName("platform")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, selection.Write(path, models.Selection{
		Version:   models.SelectionVersion,
		Owner:     "acme",
		OwnerType: models.OwnerOrganization,
		Tool:      "goldfinger test",
		Repos:     []models.Repo{{Owner: "acme", Name: "alpha"}, {Owner: "acme", Name: "beta"}},
	}, selection.WriteOptions{Overwrite: true}))

	cs := connectMCPTestClient(t)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "selections"})
	require.NoError(t, err)
	require.False(t, res.IsError, "selections must succeed offline: %v", res.Content)

	var out selectionsReport
	decodeStructured(t, res, &out)
	require.Len(t, out.Selections, 1, "the one lockfile in the registry must be listed")
	entry := out.Selections[0]
	assert.Equal(t, "platform", entry.Name)
	require.NotNil(t, entry.RepoCount, "a readable selection reports its repo count")
	assert.Equal(t, 2, *entry.RepoCount)
}

// TestMCPApplyPlanDisplayStaysRunnableWhileArgvIsSourceOfTruth locks the
// human-facing contract of the plan command: Argv is the verbatim source of truth a
// machine runs, and Display is a copy-pasteable shell line that quotes exactly the
// tokens that need it — so a human pasting Display gets the same argv back. The two
// must never drift, and shellQuote must escape correctly (embedded quotes, spaces,
// empty), or a pasted command could run something different from what was planned.
func TestMCPApplyPlanDisplayStaysRunnableWhileArgvIsSourceOfTruth(t *testing.T) {
	// shellQuote: safe-set tokens pass through unquoted; anything else is
	// single-quoted, an embedded single quote is escaped, and empty becomes ''.
	for in, want := range map[string]string{
		"s/old/new/": "s/old/new/", // conservative safe set: no quoting
		"main":       "main",
		"a b":        "'a b'",     // a space forces quoting
		"it's":       `'it'\''s'`, // embedded single quote escaped, not dropped
		"":           "''",        // empty token is explicit, not a vanished arg
	} {
		assert.Equalf(t, want, shellQuote(in), "shellQuote(%q)", in)
	}

	// Cross-surface: a script token carrying a space must survive verbatim in Argv
	// but be quoted in Display, and Display must equal mcpDisplay(Argv) exactly — so a
	// future handler cannot hand-build a Display that diverges from the real argv.
	t.Setenv(tokenEnvVar, "")
	stubGhToken(t, "", false)
	path, _ := writeTestSelection(t)
	cs := connectMCPTestClient(t)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "apply_plan",
		Arguments: map[string]any{
			"path":           path,
			"branch":         "b",
			"commit_message": "m",
			"pr_title":       "t",
			"sign":           models.SignLocal,
			"script":         []string{"sed", "-i", "s/a b/c/", "go.mod"},
		},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, "apply_plan must succeed offline: %v", res.Content)

	var out mcpApplyPlanResult
	decodeStructured(t, res, &out)
	assert.Equal(t, []string{"sed", "-i", "s/a b/c/", "go.mod"}, scriptAfterSeparator(out.LiveCommand.Argv),
		"Argv carries the space-bearing token verbatim — it is the source of truth")
	assert.Contains(t, out.LiveCommand.Display, "'s/a b/c/'",
		"Display quotes the space-bearing token so the line stays copy-pasteable")
	assert.Equal(t, mcpDisplay(out.LiveCommand.Argv), out.LiveCommand.Display,
		"Display must be exactly mcpDisplay(Argv) — the two never drift")
}

// TestMCPCheckReturnsDriftAsDataNotAnError is the drift-as-data contract: when a
// selection has drifted from live discovery, the check tool must report that as a
// SUCCESSFUL result (IsError:false, InSync:false, the removed repo listed), never
// as a protocol error. The exit-code-1-on-drift convention is a CLI shell nicety;
// over MCP an agent must be able to read the drift, not catch an exception. A fake
// listRepos drops one repo, so the whole thing runs hermetically — no token, no
// GitHub.
func TestMCPCheckReturnsDriftAsDataNotAnError(t *testing.T) {
	path, _ := writeTestSelection(t) // frozen: acme/alpha + acme/beta, topic "platform".

	// Live discovery now returns only alpha (beta was deleted). Both repos carry the
	// selection's frozen topic so the filter keeps them; the owner type is unchanged,
	// isolating the drift to a single removed repo.
	fakeList := func(_ context.Context, _ string) ([]models.Repo, string, error) {
		return []models.Repo{
			{Owner: "acme", Name: "alpha", DefaultBranch: "main", Topics: []string{"platform"}},
		}, models.OwnerOrganization, nil
	}
	deps := defaultMCPDeps()
	deps.listRepos = fakeList

	cs := connectMCPTestClientWithDeps(t, deps)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "check",
		Arguments: map[string]any{"path": path},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, "drift must be a successful result, not a protocol error: %v", res.Content)

	var out checkReport
	decodeStructured(t, res, &out)

	assert.False(t, out.InSync, "a removed repo means the selection is out of sync")
	require.Len(t, out.Removed, 1, "exactly the one deleted repo must be reported removed")
	assert.Equal(t, "acme/beta", out.Removed[0].Repo, "the removed repo must be named in the drift report")
	assert.Empty(t, out.Added, "nothing was added")
}

// TestMCPMirrorPassesTokenViaEnvNeverArgv locks the token-handling contract for
// the mirror tool end to end: the PAT must reach ghorg through its env var
// (GHORG_GITHUB_TOKEN), and NEVER appear in argv (a process listing / crash log
// leak). It fakes only the environment edges (token resolution, login, PATH
// check) and a recording delegate, but runs the REAL mirror.Mirror in between —
// so it proves the actual env/argv split the production path takes, not a
// restatement of the test's own wiring.
func TestMCPMirrorPassesTokenViaEnvNeverArgv(t *testing.T) {
	const token = "ghp_secret_pat_value" //nolint:gosec // G101: fake test token, not a real credential.
	path, _ := writeTestSelection(t)

	var gotArgs, gotEnv []string
	deps := defaultMCPDeps()
	deps.resolveToken = func(context.Context) (string, string, error) { return token, tokenEnvVar, nil }
	deps.verifyLogin = func(context.Context, string) (string, error) { return "acme-bot", nil }
	deps.requireTool = func(string, string) error { return nil } // ghorg "present".
	deps.delegate = func(_ context.Context, _ string, args, env []string, tok string) ([]byte, error) {
		gotArgs, gotEnv = args, env
		assert.Equal(t, token, tok, "the resolved token must be handed to the delegate for redaction")
		return []byte("ok"), nil
	}

	cs := connectMCPTestClientWithDeps(t, deps)
	// A dry-run clones nothing, so the handler skips on-disk reconcile/manifest and
	// the assertions stay focused on the env/argv split (still exercises real Mirror).
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "mirror",
		Arguments: map[string]any{
			"path":      path,
			"workspace": t.TempDir(),
			"dry_run":   true,
		},
	})
	require.NoError(t, err)
	require.Falsef(t, res.IsError, "mirror dry-run must succeed: %v", res.Content)

	assert.Contains(t, gotEnv, "GHORG_GITHUB_TOKEN="+token,
		"the PAT must reach ghorg through its own env var")
	for _, a := range gotArgs {
		assert.NotContainsf(t, a, token, "the PAT must never appear in argv: %q", a)
	}
	// Belt and braces: the source env var must not have carried the raw PAT through
	// either (mirror.Mirror strips it), so ghorg reads it only from GHORG_GITHUB_TOKEN.
	assert.NotContains(t, gotEnv, tokenEnvVar+"="+token,
		"the source token env var must be stripped before ghorg runs")
}

// decodeStructured round-trips a tool result's structured content into v.
func decodeStructured(t *testing.T, res *mcp.CallToolResult, v any) {
	t.Helper()
	require.NotNil(t, res.StructuredContent, "tool must return structured content")
	raw, err := json.Marshal(res.StructuredContent)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, v))
}
