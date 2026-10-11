package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// maxCapturedOutput bounds how much delegate output a runner RETAINS, so a chatty
// or runaway child cannot balloon goldfinger's memory with the size of the fleet.
// The tail is what carries a final status line or error — multi-gitter prints its
// repo-counter block last — so the buffer keeps the last maxCapturedOutput bytes
// and drops the oldest. It bounds only the retained copy; a streaming runner still
// tees every byte to the operator's terminal live.
const maxCapturedOutput = 1 << 20 // 1 MiB

// killGraceDelay bounds how long Wait blocks before os/exec force-closes the
// child's pipes: after context-cancellation, capping the time a delegate ignoring
// SIGKILL can keep a cancelled run from returning; and after a normal exit, capping
// a grandchild that outlived its parent while still holding the output pipe open.
// Without it that second case blocks Wait for as long as the grandchild lives.
const killGraceDelay = 5 * time.Second

// execRun is the real command runner passed to the mirror/apply wrappers. It
// streams the child tool's output straight through so the user sees ghorg's and
// multi-gitter's progress live. The child's stdout is deliberately routed to our
// stderr: goldfinger reserves its own stdout for machine-readable output (the
// mirror workspace path), so a delegate's chatter must never contaminate it.
func execRun(ctx context.Context, name string, args, env []string) error {
	return execRunToWriter(ctx, name, args, env, os.Stderr)
}

func execRunQuiet(ctx context.Context, name string, args, env []string) error {
	return execRunToWriter(ctx, name, args, env, io.Discard)
}

func execRunToWriter(ctx context.Context, name string, args, env []string, w io.Writer) error {
	// name/args are goldfinger's own delegate wiring (ghorg/multi-gitter + flags
	// built in-process), never unsanitised external input; the single intentional exec seam.
	c := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: see comment above — controlled delegate invocation, not external input.
	c.Env = env
	c.Stdout = w
	c.Stderr = w
	setDelegateLifecycle(c)
	return c.Run()
}

// setDelegateLifecycle applies the process-lifecycle guards every delegate run
// needs, on the CLI as much as under MCP. Keeping them in one function is the
// point: the CLI runners and the MCP runners drifted once already, with only the
// latter hardened.
//
//   - The child runs in its own process group, and cancelling the context kills
//     that whole group (see setProcessGroup). Delegates spawn children — ghorg
//     shells out to git — so killing only the direct child would orphan
//     grandchildren that keep cloning and writing after goldfinger has exited.
//   - Wait is bounded by killGraceDelay, so a stuck child cannot hang the run
//     indefinitely after cancellation.
//   - Stdin is nil (/dev/null), NOT the operator's terminal. This one is a
//     consequence of the first: a child in its own process group is no longer the
//     terminal's foreground group, so reading an inherited TTY raises SIGTTIN and
//     STOPS it — which presents as a hang. Nothing goldfinger drives reads stdin:
//     multi-gitter never wires it (so neither does the change command it runs),
//     and ghorg's only prompt is on the prune path, whose env knobs mirror scrubs
//     (models/mirror ghorg drop list). Should a delegate ever prompt anyway,
//     /dev/null answers with EOF — its safe default — instead of wedging.
func setDelegateLifecycle(c *exec.Cmd) {
	c.Stdin = nil
	setProcessGroup(c)
	c.WaitDelay = killGraceDelay
}

// execApplyRun is the apply-specific runner. Dry-runs are captured so goldfinger
// can summarize multi-gitter's final repo counter block while still teeing live
// progress to stderr. Live applies keep the existing streaming path.
func execApplyRun(ctx context.Context, name string, args, env []string) ([]byte, error) {
	return execApplyRunToWriter(ctx, name, args, env, os.Stderr)
}

func execApplyRunQuiet(ctx context.Context, name string, args, env []string) ([]byte, error) {
	return execApplyRunToWriter(ctx, name, args, env, io.Discard)
}

func execApplyRunToWriter(ctx context.Context, name string, args, env []string, w io.Writer) ([]byte, error) {
	if !hasArg(args, "--dry-run") {
		return nil, execRunToWriter(ctx, name, args, env, w)
	}

	// Only the retained copy is bounded — w still receives every byte live, so the
	// operator watching the run sees the whole thing; it is the in-memory copy that
	// must not grow with fleet size × how chatty the change command is.
	buf := &boundedBuffer{limit: maxCapturedOutput}
	// Same writer for both streams: os/exec dedups it to one pipe and one copier
	// goroutine, so there is no concurrent write to boundedBuffer.
	combined := io.MultiWriter(w, buf)
	c := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: see execRun — controlled delegate invocation, not external input.
	c.Env = env
	c.Stdout = combined
	c.Stderr = combined
	setDelegateLifecycle(c)
	err := c.Run()
	return capturedOutput(buf), err
}

// capturedOutput returns the retained bytes, prefixed with a marker whenever older
// output was dropped. The marker exists because apply prints this buffer to a file
// it labels "full run output": a silently tail-truncated log would make that label
// a lie, and an operator reading it for a repo that never appears could not tell a
// dropped line from a repo multi-gitter never reported.
//
// Both steps below defend the same thing — that truncation may destroy information
// but must never MANUFACTURE any. What reads this buffer is apply/dryrun.go's
// parser, and its grammar is positional: a line ending in ":" is a result-section
// header, and the indented lines under it take that section's status.
//
//   - The fragment left of the line truncation cut through is discarded. Half a
//     line is not data: it can end in ":" where the whole line did not, inventing a
//     section header out of some chatty change-command output. The repos listed
//     under it would then be reported as errors, with the fragment as the reason —
//     and because a real header later in the tail still sets the parser's
//     "recognised format" flag, the digest would look parseable and the misreport
//     would be silent.
//   - The marker itself ends in "." for the same reason. Once the fragment is gone
//     the tail can legitimately begin with a section's indented repo lines, its
//     header having been the discarded fragment — at which point the marker is the
//     first header-shaped line the parser meets. Ending it in ":" would bucket those
//     repos under the marker text; ending it in "." leaves them "unknown", which is
//     the truth.
//
// A test pins each (TestTruncationNeverManufacturesAResultSectionHeader and
// TestTruncationMarkerCannotBecomeAResultSectionHeader).
func capturedOutput(buf *boundedBuffer) []byte {
	out := buf.Bytes()
	if !buf.truncated {
		return out
	}
	if nl := bytes.IndexByte(out, '\n'); nl >= 0 {
		out = out[nl+1:]
	} else {
		out = nil // the whole retained tail is one unterminated fragment
	}
	marker := fmt.Sprintf("[goldfinger] earlier output dropped — retained the last %d bytes of a longer run.\n", buf.limit)
	return append([]byte(marker), out...)
}

// boundedBuffer is an io.Writer that retains only the last limit bytes written, so
// unbounded delegate output cannot exhaust memory. It records whether earlier bytes
// were dropped. It is not safe for concurrent writers, which is fine: its callers
// point both child streams at one instance and os/exec serialises them through a
// single copier.
//
// redact, when set, is applied to the whole buffer immediately before old bytes are
// dropped. This is what makes secret-masking robust against truncation: a token
// that lands across the drop boundary would otherwise lose its front half to
// truncation and survive as an unmaskable fragment. Redacting while the token is
// still whole turns it into the fixed marker first, so truncation can only ever cut
// a marker, never split a live secret. Only the MCP runner sets it (see mcpRun);
// the CLI runners write the delegate's own output back to the operator's terminal,
// which already saw it live.
type boundedBuffer struct {
	limit     int
	redact    func([]byte) []byte
	buf       []byte
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.buf = append(b.buf, p...)
	if b.limit > 0 && len(b.buf) > b.limit {
		if b.redact != nil {
			b.buf = b.redact(b.buf)
		}
		// Redaction shrinks the marker (< the token), so re-check before slicing.
		if len(b.buf) > b.limit {
			b.truncated = true
			b.buf = b.buf[len(b.buf)-b.limit:]
		}
	}
	return n, nil
}

func (b *boundedBuffer) Bytes() []byte { return b.buf }

// newGhorgLog creates the 0600 temp file that captures a mirror run's full ghorg
// output (WS3 of #48). Like apply's captured output it is a persistent drill-down
// artifact — the caller closes the handle when ghorg finishes but does not remove
// the file, so an operator can inspect clone errors after the terse summary.
func newGhorgLog() (*os.File, error) {
	f, err := os.CreateTemp("", "goldfinger-mirror-output-*.log")
	if err != nil {
		return nil, fmt.Errorf("create mirror output log: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, fmt.Errorf("secure mirror output log perms: %w", err)
	}
	return f, nil
}

func hasArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

// requireTool fails with an install hint if the named CLI is not on PATH. Both
// tools goldfinger drives are `go install`-ed, which drops them in the Go bin
// dir — a spot that's frequently missing from PATH. So before telling the user
// to reinstall, we check there: if the binary exists but just isn't on PATH,
// the fix is a PATH export, not another install.
func requireTool(name, installHint string) error {
	if _, err := exec.LookPath(name); err == nil {
		return nil
	}
	if dir := goBinDirContaining(name); dir != "" {
		return fmt.Errorf("%s is installed at %s but that directory is not on PATH — add it: export PATH=\"$PATH:%s\"", name, filepath.Join(dir, name), dir)
	}
	return fmt.Errorf("%s is required but not found on PATH — install it: %s", name, installHint)
}

// goBinDirContaining returns the Go bin directory that holds an executable named
// `name`, or "" if none does. It mirrors `go`'s own resolution order (GOBIN,
// then each GOPATH's bin, then the default ~/go/bin) without shelling out to the
// go toolchain, which may not itself be on PATH.
func goBinDirContaining(name string) string {
	var dirs []string
	if gobin := os.Getenv("GOBIN"); gobin != "" {
		dirs = append(dirs, gobin)
	}
	for _, gopath := range filepath.SplitList(os.Getenv("GOPATH")) {
		if gopath != "" {
			dirs = append(dirs, filepath.Join(gopath, "bin"))
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, "go", "bin"))
	}
	for _, dir := range dirs {
		info, err := os.Stat(filepath.Join(dir, name)) //nolint:gosec // G703: dir is a Go bin dir from the environment, name is a fixed tool name; a read-only Stat for a PATH-style lookup, no file contents opened.
		if err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return dir
		}
	}
	return ""
}
