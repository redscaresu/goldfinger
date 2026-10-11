//go:build unix

package main

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestInterruptContextCancelsOnHangup sends the real signal to this process. A
// terminal hangup must cancel the context — the only thing that kills the
// delegates' process groups — rather than kill goldfinger and orphan them.
func TestInterruptContextCancelsOnHangup(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGQUIT} {
		t.Run(sig.String(), func(t *testing.T) {
			ctx, restore := interruptContext(context.Background())
			defer restore()
			assert.NoError(t, syscall.Kill(syscall.Getpid(), sig))
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
				t.Fatalf("%v did not cancel the root context", sig)
			}
		})
	}
}
