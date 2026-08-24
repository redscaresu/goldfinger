package main

import (
	"bytes"
	"context"
	"os/exec"
)

// mcpRun executes a delegate tool in a way that is safe to call from inside the
// MCP stdio server. It adds one property to the lifecycle guards every delegate
// run gets (setDelegateLifecycle: own process group, bounded Wait, no inherited
// stdin): the child never touches the server's stdout/stderr either. Those are the
// JSON-RPC channel; a byte of ghorg chatter on stdout would corrupt the protocol,
// so output is captured into a bounded buffer instead of streamed.
//
// It returns the child's combined output (tail-truncated). token is redacted
// from the captured output — including across the truncation boundary, see
// boundedBuffer — so a secret can never survive in the returned bytes; pass ""
// when there is no secret to mask.
func mcpRun(ctx context.Context, name string, args, env []string, token string) ([]byte, error) {
	c := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: controlled delegate invocation (ghorg/multi-gitter + in-process flags), not external input — same seam as execRun.
	c.Env = env
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
	setDelegateLifecycle(c)
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
// the same lifecycle guards as mcpRun (no stdin, own process group killed as a
// whole on context-cancel, bounded Wait). It exists for the preflight
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
	buf := &boundedBuffer{limit: maxCapturedOutput}
	c.Stdout = buf
	c.Stderr = nil // discard stderr to the null device: only stdout is parsed, and stderr noise must not corrupt it.
	setDelegateLifecycle(c)
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
