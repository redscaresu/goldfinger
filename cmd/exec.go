package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

// execRun is the real command runner passed to the mirror/apply wrappers. It
// streams the child tool's output straight through so the user sees ghorg's and
// multi-gitter's progress live.
func execRun(ctx context.Context, name string, args, env []string) error {
	c := exec.CommandContext(ctx, name, args...)
	c.Env = env
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	c.Stdin = os.Stdin
	return c.Run()
}

// requireTool fails with an install hint if the named CLI is not on PATH.
func requireTool(name, installHint string) error {
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("%s is required but not found on PATH — install it: %s", name, installHint)
	}
	return nil
}
