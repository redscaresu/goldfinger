//go:build !unix

package main

import "os/exec"

// setProcessGroup is a no-op on platforms without POSIX process groups; the
// default CommandContext cancellation (SIGKILL to the direct child) and mcpRun's
// WaitDelay still apply. goldfinger delegates to unix tooling (ghorg, git), so
// this exists to keep the package compiling on other GOOS, not as a supported
// runtime path.
func setProcessGroup(c *exec.Cmd) { _ = c }
