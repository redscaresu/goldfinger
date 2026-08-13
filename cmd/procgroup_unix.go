//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in a new process group (it becomes the group
// leader, pgid == pid) and rewires context-cancellation to kill that whole
// group. Delegate tools spawn children — ghorg shells out to git — so killing
// only the direct child on cancel would orphan those grandchildren, which keep
// running and writing after the MCP tool call has returned. Signalling the
// negative pid targets the group, reaping the tree.
func setProcessGroup(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		if c.Process == nil {
			return nil
		}
		// Negative pid => the process group led by the child.
		return syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
}
