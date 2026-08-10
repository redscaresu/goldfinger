//go:build unix

package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMCPRunCapturesBothStreamsNeverStdio(t *testing.T) {
	// A delegate writes to both stdout and stderr. mcpRun must capture both into
	// the returned buffer and route neither to the process's real stdout/stderr
	// (which, in the server, are the JSON-RPC channel). c.Stdout/c.Stderr are set
	// to the bounded buffer, so capturing both here proves nothing reached stdio.
	out, err := mcpRun(context.Background(), "sh", []string{"-c", "echo to-stdout; echo to-stderr 1>&2"}, nil, "")
	require.NoError(t, err)
	s := string(out)
	assert.Contains(t, s, "to-stdout")
	assert.Contains(t, s, "to-stderr")
}

func TestMCPRunStdinIsNilNotServerStdin(t *testing.T) {
	// With Stdin nil the child gets /dev/null, so a reader hits EOF immediately
	// instead of blocking on the server's real stdin. If stdin were inherited,
	// `cat` would block and the test would hang until the deadline.
	done := make(chan struct{})
	go func() {
		defer close(done)
		out, err := mcpRun(context.Background(), "sh", []string{"-c", "cat; echo done"}, nil, "")
		assert.NoError(t, err)
		assert.Contains(t, string(out), "done")
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("mcpRun blocked on stdin — child did not get /dev/null")
	}
}

func TestBoundedBufferKeepsTailAndFlagsTruncation(t *testing.T) {
	b := &boundedBuffer{limit: 4}
	_, _ = b.Write([]byte("abcdef"))
	assert.Equal(t, "cdef", string(b.Bytes()), "keeps the last limit bytes")
	assert.True(t, b.truncated)

	b2 := &boundedBuffer{limit: 8}
	_, _ = b2.Write([]byte("hi"))
	assert.Equal(t, "hi", string(b2.Bytes()))
	assert.False(t, b2.truncated)
}

func TestBoundedBufferRedactsTokenAcrossTruncationBoundary(t *testing.T) {
	// The boundary case: a token lands where "keep the last N bytes" would slice
	// through it. Redacting only after truncation would leave an unmaskable
	// fragment (its front half dropped); redacting inside the buffer, before the
	// drop, masks it while still whole. Assert the full token never survives no
	// matter how later writes push it toward the drop boundary.
	const token = "SUPERSECRETTOKEN"
	b := &boundedBuffer{limit: 16, redact: func(p []byte) []byte { return redactToken(p, token) }}
	_, _ = b.Write([]byte("xxxxxxxxxxxx")) // 12 bytes, under the limit
	_, _ = b.Write([]byte(token))          // token completes here, overflowing the buffer
	_, _ = b.Write([]byte("yyyyyyyyyyyy")) // push it across the drop boundary

	got := string(b.Bytes())
	assert.NotContains(t, got, token, "the full token must never survive truncation")
	assert.NotContains(t, got, "SECRET", "no live fragment of the token may survive")
	assert.True(t, b.truncated)
}

func TestRedactToken(t *testing.T) {
	assert.Equal(t, "a [REDACTED] b [REDACTED]", string(redactToken([]byte("a s3cr3t b s3cr3t"), "s3cr3t")))
	// Empty token is a no-op — never redact the whole world.
	assert.Equal(t, "unchanged", string(redactToken([]byte("unchanged"), "")))
}

func TestMCPDelegateRedactsEchoedToken(t *testing.T) {
	// The safety property: even a delegate that dumps its token env var must not
	// leak the PAT through mcpDelegate's returned output.
	const token = "ghp_pretend_secret_value"
	env := []string{"GHORG_GITHUB_TOKEN=" + token}
	out, err := mcpDelegate(context.Background(), "sh",
		[]string{"-c", "echo token is $GHORG_GITHUB_TOKEN"}, env, token)
	require.NoError(t, err)
	assert.NotContains(t, string(out), token, "the PAT must never survive in returned output")
	assert.Contains(t, string(out), "[REDACTED]")
}

func TestMCPRunKillsProcessGroupOnCancel(t *testing.T) {
	// The child (sh) backgrounds a long sleep — a grandchild in the same process
	// group — and records its PID, then waits. On cancel, killing only the direct
	// child would orphan the sleep; a process-group kill reaps it. We assert the
	// grandchild is gone.
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	script := "sleep 60 & echo $! > " + pidFile + "; wait"

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = mcpRun(ctx, "sh", []string{"-c", script}, nil, "")
	}()

	pid := waitForPID(t, pidFile)
	assert.True(t, processAlive(pid), "grandchild should be running before cancel")

	cancel()
	<-done

	require.Eventually(t, func() bool { return !processAlive(pid) }, 10*time.Second, 20*time.Millisecond,
		"grandchild survived cancel — process group was not killed")
}

// waitForPID polls pidFile until it holds a parseable PID, or fails on timeout.
func waitForPID(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidFile) //nolint:gosec // G304: test-created temp path.
		if err == nil {
			if pid, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("grandchild never recorded its PID to %s", pidFile)
	return 0
}

// processAlive reports whether a PID is still live (signal 0 probes existence).
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
