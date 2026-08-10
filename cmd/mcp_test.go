package main

import (
	"context"
	"encoding/json"
	"path/filepath"
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
	t.Helper()
	ctx := context.Background()
	serverT, clientT := mcp.NewInMemoryTransports()

	_, err := newMCPServer().Connect(ctx, serverT, nil)
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
		"apply_plan", "workspaces_list", "doctor",
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

// decodeStructured round-trips a tool result's structured content into v.
func decodeStructured(t *testing.T, res *mcp.CallToolResult, v any) {
	t.Helper()
	require.NotNil(t, res.StructuredContent, "tool must return structured content")
	raw, err := json.Marshal(res.StructuredContent)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, v))
}
