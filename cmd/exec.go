package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
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
	return execRunToWriter(ctx, name, args, env, os.Stderr)
}

func execRunQuiet(ctx context.Context, name string, args, env []string) error {
	return execRunToWriter(ctx, name, args, env, io.Discard)
}

func execRunToWriter(ctx context.Context, name string, args, env []string, w io.Writer) error {
	// name/args are goldfinger's own delegate wiring (ghorg/multi-gitter + flags
	// built in-process), never unsanitised external input; the single intentional exec seam.
	c := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: see comment above — controlled delegate invocation, not external input.
	c.Env = env
	c.Stdout = w
	c.Stderr = w
	c.Stdin = os.Stdin
	return c.Run()
}

// execApplyRun is the apply-specific runner. Dry-runs are captured so goldfinger
// can summarize multi-gitter's final repo counter block while still teeing live
// progress to stderr. Live applies keep the existing streaming path.
func execApplyRun(ctx context.Context, name string, args, env []string) ([]byte, error) {
	return execApplyRunToWriter(ctx, name, args, env, os.Stderr)
}

func execApplyRunQuiet(ctx context.Context, name string, args, env []string) ([]byte, error) {
	return execApplyRunToWriter(ctx, name, args, env, io.Discard)
}

func execApplyRunToWriter(ctx context.Context, name string, args, env []string, w io.Writer) ([]byte, error) {
	if !hasArg(args, "--dry-run") {
		return nil, execRunToWriter(ctx, name, args, env, w)
	}

	var buf bytes.Buffer
	combined := io.MultiWriter(w, &buf)
	c := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: see execRun — controlled delegate invocation, not external input.
	c.Env = env
	c.Stdout = combined
	c.Stderr = combined
	c.Stdin = os.Stdin
	err := c.Run()
	return buf.Bytes(), err
}

// newGhorgLog creates the 0600 temp file that captures a mirror run's full ghorg
// output (WS3 of #48). Like apply's captured output it is a persistent drill-down
// artifact — the caller closes the handle when ghorg finishes but does not remove
// the file, so an operator can inspect clone errors after the terse summary.
func newGhorgLog() (*os.File, error) {
	f, err := os.CreateTemp("", "goldfinger-mirror-output-*.log")
	if err != nil {
		return nil, fmt.Errorf("create mirror output log: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, fmt.Errorf("secure mirror output log perms: %w", err)
	}
	return f, nil
}

func hasArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
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
		info, err := os.Stat(filepath.Join(dir, name)) //nolint:gosec // G703: dir is a Go bin dir from the environment, name is a fixed tool name; a read-only Stat for a PATH-style lookup, no file contents opened.
		if err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return dir
		}
	}
	return ""
}
