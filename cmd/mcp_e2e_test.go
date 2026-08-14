package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/redscaresu/goldfinger/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildGoldfingerBinary compiles the real goldfinger binary to a temp path so the
// e2e can launch it as a child process. -mod=readonly matches how CI and the
// Makefile build; the module cache is already populated, so this stays offline.
func buildGoldfingerBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "goldfinger")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	out, err := exec.Command("go", "build", "-mod=readonly", "-o", bin,
		"github.com/redscaresu/goldfinger/cmd").CombinedOutput()
	require.NoErrorf(t, err, "build goldfinger: %s", out)
	return bin
}

// TestMCPServerSubprocessSpeaksCleanStdioJSONRPC is the Tier-3 e2e: it launches the
// real `goldfinger mcp` binary and drives it over its ACTUAL stdin/stdout pipes.
//
// This guards the server's single most important invariant — that nothing but
// JSON-RPC ever reaches the process's real stdout — which the in-memory transport
// tests structurally cannot: those never touch os.Stdout, so a stray fmt.Println in
// any handler would sail straight past them. Here, CommandTransport parses
// newline-delimited JSON from the child's real stdout, so a single non-JSON byte on
// that stream corrupts a frame and surfaces as a decode error — either on the call
// that provoked it, or, for a write during shutdown, from Wait() at teardown.
//
// Note this asserts each call DECODES (a clean JSON-RPC round-trip = clean stdout),
// not that it succeeds: IsError semantics are the in-memory tests' job. It exercises
// every OFFLINE read-and-plan handler; the four network tools (check/select/mirror/
// doctor) can't run hermetically and are out of this e2e's scope by design.
func TestMCPServerSubprocessSpeaksCleanStdioJSONRPC(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary and spawns a subprocess; skipped in -short")
	}
	bin := buildGoldfingerBinary(t)
	selPath, _ := writeTestSelection(t) // fixture for the apply_plan call below.

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	child := exec.Command(bin, "mcp")
	// Keep the child fully offline: no token, no inherited GitHub creds. The
	// read-and-plan handlers below are pure and need none — so a green run also
	// reproves the surface answers without reaching the network.
	child.Env = append(os.Environ(), tokenEnvVar+"=", "GITHUB_TOKEN=", "GH_TOKEN=")
	// stderr is not the protocol channel (banners/errors are allowed there); capture
	// it to a file so a handshake failure reports what the child actually said. An
	// *os.File is handed to the child as fd 2 directly — no os/exec copier goroutine,
	// so reading it for a diagnostic can't race the capture (unlike a bytes.Buffer).
	stderrPath := filepath.Join(filepath.Dir(bin), "child-stderr.log")
	stderrFile, err := os.Create(stderrPath) //nolint:gosec // G304: test-owned temp path.
	require.NoError(t, err)
	t.Cleanup(func() { _ = stderrFile.Close() })
	child.Stderr = stderrFile
	childStderr := func() string { b, _ := os.ReadFile(stderrPath); return string(b) } //nolint:gosec // G304: test-owned temp path.

	client := mcp.NewClient(&mcp.Implementation{Name: "e2e-client", Version: "0"}, nil)

	// Connect runs the MCP `initialize` handshake over the child's real stdio.
	cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: child}, nil)
	require.NoErrorf(t, err, "initialize over subprocess stdio (child stderr: %s)", childStderr())
	// Safety net if an assertion below aborts the test; the happy path closes
	// explicitly and asserts a clean teardown.
	t.Cleanup(func() { _ = cs.Close() })

	// tools/list over the wire: a successful decode proves stdout carried only valid
	// newline-delimited JSON-RPC frames, and the charter spine holds over the real
	// transport too (guide present, apply never a tool).
	listed, err := cs.ListTools(ctx, nil)
	require.NoErrorf(t, err, "tools/list over subprocess (child stderr: %s)", childStderr())
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	assert.Contains(t, names, "guide", "the read-and-plan surface must answer over the real transport")
	assert.NotContains(t, names, "apply", "apply must never be a tool, even over the real transport")

	// Exercise every OFFLINE handler over the real transport, so each proves it wrote
	// nothing but JSON-RPC to the real stdout — not just the startup/list path.
	offlineCalls := []struct {
		name string
		args map[string]any
	}{
		{name: "guide"},
		{name: "schema"},
		{name: "selections"},
		{name: "workspaces_list"},
		// scan is the newest offline read-and-plan handler and, unlike the others,
		// walks the filesystem and (in the CLI path) prints stderr warnings — exactly
		// the shape of handler this stdio-cleanliness guard exists for. An empty
		// workspace means every selected repo reports scanned:false, so the call is
		// deterministic and offline (no mirror needed) while still driving the handler.
		{name: "scan", args: map[string]any{
			"path":      selPath,
			"pattern":   "goldfinger",
			"workspace": t.TempDir(),
		}},
		{name: "apply_plan", args: map[string]any{
			"path":           selPath,
			"branch":         "bump-dep",
			"commit_message": "bump dep",
			"pr_title":       "Bump dep",
			"sign":           models.SignLocal,
			"script":         []string{"true"},
		}},
	}
	for _, c := range offlineCalls {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: c.name, Arguments: c.args})
		require.NoErrorf(t, err, "tools/call %s over subprocess (child stderr: %s)", c.name, childStderr())
		assert.NotNilf(t, res, "%s returned a nil result over the real transport", c.name)
	}

	// Teardown must ALSO be stdout-clean. Closing the client sends stdin EOF, so the
	// server's Run returns and the child exits; Wait then reports any read-side
	// decode error — the signal for a stray stdout write during shutdown, after the
	// last response above. Close alone swallows that error, so assert on Wait.
	require.NoError(t, cs.Close(), "client close")
	require.NoErrorf(t, cs.Wait(),
		"server terminated with a read error — stray stdout on shutdown? (child stderr: %s)", childStderr())
}
