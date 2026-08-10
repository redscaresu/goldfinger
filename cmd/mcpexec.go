package main

import (
	"bytes"
	"context"
	"os/exec"
	"time"
)

// maxCapturedOutput bounds how much delegate output the MCP runner retains, so a
// chatty or runaway child cannot balloon the long-lived server's memory. The
// tail is what carries a final status line or error, so the buffer keeps the
// last maxCapturedOutput bytes and drops the oldest.
const maxCapturedOutput = 1 << 20 // 1 MiB

// killGraceDelay bounds how long Wait blocks after context-cancellation before
// os/exec force-closes the child's pipes. It caps the time a delegate ignoring
// SIGKILL (or a wedged grandchild holding the pipe) can keep an MCP tool call
// from returning.
const killGraceDelay = 5 * time.Second

// mcpRun executes a delegate tool in a way that is safe to call from inside the
// MCP stdio server. Three properties matter, none of which the streaming CLI
// runners provide:
//
//   - The child NEVER touches the server's stdin/stdout/stderr. Those are the
//     JSON-RPC channel; a byte of ghorg chatter on stdout would corrupt the
//     protocol. Output is captured into a bounded buffer instead, and stdin is
//     nil (a child that blocked reading it would deadlock the server).
//   - The child runs in its own process group and context-cancellation kills the
//     whole group (see setProcessGroup), so cancelling a tool call reaps the
//     delegate's grandchildren (ghorg → git) rather than orphaning processes that
//     keep running and writing after the call returns.
//   - Wait is bounded by killGraceDelay so a stuck child can't wedge the call.
//
// It returns the child's combined output (tail-truncated). Redacting secrets is
// the caller's job via mcpDelegate/redactToken — only the caller knows the token.
func mcpRun(ctx context.Context, name string, args, env []string) ([]byte, error) {
	c := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: controlled delegate invocation (ghorg/multi-gitter + in-process flags), not external input — same seam as execRun.
	c.Env = env
	c.Stdin = nil
	buf := &boundedBuffer{limit: maxCapturedOutput}
	// Same writer for both streams: os/exec dedups it to one pipe and one copier
	// goroutine, so there is no concurrent write to boundedBuffer.
	c.Stdout = buf
	c.Stderr = buf
	setProcessGroup(c)
	c.WaitDelay = killGraceDelay
	err := c.Run()
	return buf.Bytes(), err
}

// mcpDelegate is the combination MCP handlers should use: run the delegate, then
// redact the token from the captured output before it is ever returned to a
// caller. Keeping the redaction here (not at each call site) means a new handler
// cannot forget it.
func mcpDelegate(ctx context.Context, name string, args, env []string, token string) ([]byte, error) {
	out, err := mcpRun(ctx, name, args, env)
	return redactToken(out, token), err
}

// redactToken masks every occurrence of the token in b, so captured delegate
// output returned to an MCP caller can never leak the PAT even if a child echoes
// it (an error dumping its environment, a token-in-URL clone failure). A no-op
// when token is empty.
func redactToken(b []byte, token string) []byte {
	if token == "" {
		return b
	}
	return bytes.ReplaceAll(b, []byte(token), []byte("[REDACTED]"))
}

// boundedBuffer is an io.Writer that retains only the last limit bytes written,
// so unbounded delegate output cannot exhaust memory. It records whether earlier
// bytes were dropped. It is not safe for concurrent writers, which is fine:
// mcpRun points both child streams at one instance and os/exec serialises them
// through a single copier.
type boundedBuffer struct {
	limit     int
	buf       []byte
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.buf = append(b.buf, p...)
	if b.limit > 0 && len(b.buf) > b.limit {
		b.truncated = true
		b.buf = b.buf[len(b.buf)-b.limit:]
	}
	return n, nil
}

func (b *boundedBuffer) Bytes() []byte { return b.buf }
