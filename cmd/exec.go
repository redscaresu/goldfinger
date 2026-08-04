package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// execRun is the real command runner passed to the mirror/apply wrappers. It
// streams the child tool's output straight through so the user sees ghorg's and
// multi-gitter's progress live. The child's stdout is deliberately routed to our
// stderr: goldfinger reserves its own stdout for machine-readable output (the
// mirror workspace path), so a delegate's chatter must never contaminate it.
func execRun(ctx context.Context, name string, args, env []string) error {
	c := exec.CommandContext(ctx, name, args...)
	c.Env = env
	c.Stdout = os.Stderr
	c.Stderr = os.Stderr
	c.Stdin = os.Stdin
	return c.Run()
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
		info, err := os.Stat(filepath.Join(dir, name))
		if err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return dir
		}
	}
	return ""
}
