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
// It returns the child's combined output (tail-truncated). token is redacted
// from the captured output — including across the truncation boundary, see
// boundedBuffer — so a secret can never survive in the returned bytes; pass ""
// when there is no secret to mask.
func mcpRun(ctx context.Context, name string, args, env []string, token string) ([]byte, error) {
	c := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: controlled delegate invocation (ghorg/multi-gitter + in-process flags), not external input — same seam as execRun.
	c.Env = env
	c.Stdin = nil
	buf := &boundedBuffer{limit: maxCapturedOutput}
	if token != "" {
		// Redact inside the buffer, before it drops old bytes, so a token straddling
		// the truncation boundary is masked while still whole (see boundedBuffer).
		buf.redact = func(b []byte) []byte { return redactToken(b, token) }
	}
	// Same writer for both streams: os/exec dedups it to one pipe and one copier
	// goroutine, so there is no concurrent write to boundedBuffer.
	c.Stdout = buf
	c.Stderr = buf
	setProcessGroup(c)
	c.WaitDelay = killGraceDelay
	err := c.Run()
	// Final pass masks any token in the last, never-truncated bytes too.
	return redactToken(buf.Bytes(), token), err
}

// mcpDelegate is the combination MCP handlers should use: run the delegate with
// token redaction wired into the capture buffer, so a new handler cannot forget
// to mask the secret and a token can never leak — not even a fragment split by
// the buffer's tail-truncation.
func mcpDelegate(ctx context.Context, name string, args, env []string, token string) ([]byte, error) {
	return mcpRun(ctx, name, args, env, token)
}

// mcpProbe runs a short helper command and returns ONLY its stdout, bounded, with
// the same stdio-safety lifecycle guards as mcpRun (no stdin, own process group
// killed as a whole on context-cancel, bounded Wait). It exists for the preflight
// probes — a tool `version` line, `gh auth token` — that goldfinger runs even
// while serving MCP, where a raw exec.Command().Output() would be unsafe: the
// context kills only the direct child, so a spawned helper/grandchild holding the
// output pipe open could wedge the long-lived server, and unbounded output could
// balloon its memory.
//
// Unlike mcpRun it does NOT fold in stderr: these callers parse stdout exactly (a
// version string, a token), and stderr noise (a gh update notice, a deprecation
// warning) must not corrupt that value — stderr goes to the null device. There is
// no token redaction: the probe env is either scrubbed of secrets (doctor) or the
// stdout IS the secret the caller needs verbatim (gh auth token), so the caller,
// not this runner, owns never logging it. env scopes the child's environment
// (nil = inherit the parent's).
func mcpProbe(ctx context.Context, name string, args, env []string) ([]byte, error) {
	c := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: a fixed subcommand on a PATH-resolved tool (ghorg/multi-gitter/gh), not external input — same controlled seam as mcpRun.
	c.Env = env
	c.Stdin = nil
	buf := &boundedBuffer{limit: maxCapturedOutput}
	c.Stdout = buf
	c.Stderr = nil // discard stderr to the null device: only stdout is parsed, and stderr noise must not corrupt it.
	setProcessGroup(c)
	c.WaitDelay = killGraceDelay
	err := c.Run()
	return buf.Bytes(), err
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
//
// redact, when set, is applied to the whole buffer immediately before old bytes
// are dropped. This is what makes secret-masking robust against truncation: a
// token that lands across the drop boundary would otherwise lose its front half
// to truncation and survive as an unmaskable fragment. Redacting while the token
// is still whole turns it into the fixed marker first, so truncation can only
// ever cut a marker, never split a live secret.
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
