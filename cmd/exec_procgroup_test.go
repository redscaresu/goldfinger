//go:build unix

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The CLI is the path most likely to be interrupted — a fleet mirror is minutes
// of cloning and Ctrl-C is the obvious way to stop it — yet only the MCP runners
// were hardened against orphaning. These are the CLI counterparts of
// TestMCPRunKillsProcessGroupOnCancel: the child backgrounds a long sleep (a
// grandchild, standing in for ghorg's git), records its PID and waits. Killing
// only the direct child on cancel would leave that sleep running, still writing
// into the workspace after goldfinger returned; a process-group kill reaps it.

func TestExecRunKillsProcessGroupOnCancel(t *testing.T) {
	pid, cancel, done := startExecRunWithGrandchild(t, func(ctx context.Context, script string) {
		_ = execRunQuiet(ctx, "sh", []string{"-c", script}, os.Environ())
	})

	cancel()
	<-done

	require.Eventually(t, func() bool { return !processAlive(pid) }, 10*time.Second, 20*time.Millisecond,
		"grandchild survived cancel — the CLI runner did not kill the process group")
}

func TestExecApplyRunKillsProcessGroupOnCancel(t *testing.T) {
	// The dry-run capture path is a second exec site with its own wiring, so it is
	// the one that silently drifts. --dry-run in argv selects it (see hasArg).
	pid, cancel, done := startExecRunWithGrandchild(t, func(ctx context.Context, script string) {
		_, _ = execApplyRunQuiet(ctx, "sh", []string{"-c", script, "--dry-run"}, os.Environ())
	})

	cancel()
	<-done

	require.Eventually(t, func() bool { return !processAlive(pid) }, 10*time.Second, 20*time.Millisecond,
		"grandchild survived cancel — the apply capture runner did not kill the process group")
}

// startExecRunWithGrandchild launches run in the background with a script that
// backgrounds a sleep and records its PID, then blocks until that grandchild is
// confirmed alive. It returns the grandchild's PID, the run's cancel func, and a
// channel closed when the run returns.
func startExecRunWithGrandchild(t *testing.T, run func(ctx context.Context, script string)) (int, context.CancelFunc, chan struct{}) {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	script := "sleep 60 & echo $! > " + pidFile + "; wait"

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		run(ctx, script)
	}()

	pid := waitForPID(t, pidFile)
	assert.True(t, processAlive(pid), "grandchild should be running before cancel")
	return pid, cancel, done
}

func TestExecRunStdinIsNotTheOperatorsTerminal(t *testing.T) {
	// The child runs in its own process group, so it is not the terminal's
	// foreground group: a read from an inherited TTY would raise SIGTTIN and STOP
	// it — a hang, presented to the operator as a delegate that just went quiet.
	//
	// `go test` hands the binary /dev/null, so a child that inherited stdin would
	// pass this test for the wrong reason. os.Stdin is therefore replaced with the
	// read end of a pipe whose writer is held open: a `cat` that inherited it would
	// block forever and this test would fail on the timeout, which is exactly the
	// failure an operator would see. With Stdin nil the child reads /dev/null and
	// hits EOF at once.
	r, w, err := os.Pipe()
	require.NoError(t, err)
	orig := os.Stdin
	os.Stdin = r
	defer func() {
		os.Stdin = orig
		_ = w.Close()
		_ = r.Close()
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		assert.NoError(t, execRunQuiet(context.Background(), "sh", []string{"-c", "cat; echo done"}, os.Environ()))
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("execRun blocked on stdin — the child inherited the operator's terminal instead of /dev/null")
	}
}

// TestSetDelegateLifecycleAppliesAllThreeGuards asserts the guards directly, since
// each one's absence fails in a different, slow way: an orphaned process tree, a
// wedged Wait, a child stopped on SIGTTIN. Cancel and SysProcAttr are the
// process-group half (see procgroup_unix.go), which is why this test is unix-only.
func TestSetDelegateLifecycleAppliesAllThreeGuards(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := exec.CommandContext(ctx, "true")
	c.Stdin = os.Stdin

	setDelegateLifecycle(c)

	assert.Nil(t, c.Stdin, "an inherited terminal would raise SIGTTIN once the child is in its own process group")
	assert.Equal(t, killGraceDelay, c.WaitDelay, "Wait must be bounded so a stuck child cannot hang the run")
	assert.NotNil(t, c.Cancel, "cancellation must kill the whole group, not just the direct child")
	require.NotNil(t, c.SysProcAttr)
	assert.True(t, c.SysProcAttr.Setpgid, "the child needs its own process group for a group kill to be possible")
}
