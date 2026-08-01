// Package mirror clones a selection into a local workspace by shelling out to
// ghorg. goldfinger owns the selection; ghorg owns the cloning.
package mirror

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/redscaresu/goldfinger/models"
)

// tokenEnv is the environment variable ghorg reads its GitHub PAT from.
const tokenEnv = "GHORG_GITHUB_TOKEN"

// Runner executes an external command. It is the seam that lets Mirror build and
// dispatch a ghorg invocation without ghorg installed during tests.
type Runner func(ctx context.Context, name string, args, env []string) error

// Options are the passthrough knobs for a mirror run.
type Options struct {
	Workspace   string // ghorg --path (absolute); ghorg clones into <workspace>/<owner>
	Concurrency int    // 0 = ghorg default
	CloneDepth  int    // 0 = full history
	DryRun      bool
}

// Mirror clones exactly the repos in s into the workspace via ghorg. The token
// is passed through the child environment (never argv), so it cannot leak into
// process listings or error output.
func Mirror(ctx context.Context, run Runner, s models.Selection, token string, opts Options) error {
	if len(s.Repos) == 0 {
		return errors.New("selection is empty — nothing to mirror")
	}
	namesFile, cleanup, err := writeNamesFile(s.Repos)
	if err != nil {
		return err
	}
	defer cleanup()

	args := buildArgs(s, namesFile, opts)
	env := append(os.Environ(), tokenEnv+"="+token)
	if err := run(ctx, "ghorg", args, env); err != nil {
		return fmt.Errorf("ghorg clone %s: %w", s.Owner, err)
	}
	return nil
}

// buildArgs constructs the ghorg argv. Kept pure for unit testing.
func buildArgs(s models.Selection, namesFile string, opts Options) []string {
	args := []string{
		"clone", s.Owner,
		"--clone-type=" + cloneType(s.OwnerType),
		"--target-repos-path=" + namesFile,
	}
	if opts.Workspace != "" {
		args = append(args, "--path="+opts.Workspace)
	}
	if opts.Concurrency > 0 {
		args = append(args, "--concurrency="+strconv.Itoa(opts.Concurrency))
	}
	if opts.CloneDepth > 0 {
		args = append(args, "--clone-depth="+strconv.Itoa(opts.CloneDepth))
	}
	if opts.DryRun {
		args = append(args, "--dry-run")
	}
	return args
}

// cloneType maps a stored owner type to ghorg's --clone-type value.
func cloneType(ownerType string) string {
	if ownerType == models.OwnerOrganization {
		return "org"
	}
	return "user"
}

// writeNamesFile writes the repo names (basenames — ghorg matches on name) to a
// temp file for --target-repos-path, returning a cleanup func.
func writeNamesFile(repos []models.Repo) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "goldfinger-mirror-*.txt")
	if err != nil {
		return "", nil, fmt.Errorf("create names file: %w", err)
	}
	var b strings.Builder
	for _, r := range repos {
		b.WriteString(r.Name)
		b.WriteByte('\n')
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", nil, fmt.Errorf("write names file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", nil, fmt.Errorf("close names file: %w", err)
	}
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}
