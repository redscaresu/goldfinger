// Package apply runs a change across a selection by shelling out to
// multi-gitter. goldfinger owns the selection; multi-gitter owns the
// clone→script→commit→push→PR.
package apply

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/redscaresu/goldfinger/models"
)

// tokenEnv is the environment variable multi-gitter reads its GitHub PAT from.
const tokenEnv = "GITHUB_TOKEN"

// maxRepos caps how many repos we pass as repeated --repo flags. multi-gitter
// has no repo-list-file input, so a very large set would risk an over-length
// command line (E2BIG). Refuse loudly rather than truncate silently.
const maxRepos = 1000

// Runner executes an external command. It is the seam that lets Apply build and
// dispatch a multi-gitter invocation without multi-gitter installed in tests.
type Runner func(ctx context.Context, name string, args, env []string) error

// Apply runs spec's script across exactly the repos in s via multi-gitter. The
// token is passed through the child environment, never argv.
func Apply(ctx context.Context, run Runner, s models.Selection, spec models.ApplySpec, token string) error {
	if len(s.Repos) == 0 {
		return errors.New("selection is empty — nothing to apply")
	}
	if len(s.Repos) > maxRepos {
		return fmt.Errorf("selection has %d repos, above the %d-repo limit for a single apply (multi-gitter takes repos as command-line flags); narrow the selection", len(s.Repos), maxRepos)
	}

	scriptPath, cleanup, err := writeScript(spec.Script)
	if err != nil {
		return err
	}
	defer cleanup()

	args := buildArgs(s, spec, scriptPath)
	env := append(os.Environ(), tokenEnv+"="+token)
	if err := run(ctx, "multi-gitter", args, env); err != nil {
		return fmt.Errorf("multi-gitter run: %w", err)
	}
	return nil
}

// buildArgs constructs the multi-gitter argv. Kept pure for unit testing.
func buildArgs(s models.Selection, spec models.ApplySpec, scriptPath string) []string {
	args := []string{"run", scriptPath}
	for _, r := range s.Repos {
		args = append(args, "--repo="+r.FullName())
	}
	args = append(args,
		"--branch="+spec.Branch,
		"--commit-message="+spec.CommitMessage,
		"--pr-title="+spec.PRTitle,
	)
	if spec.PRBody != "" {
		args = append(args, "--pr-body="+spec.PRBody)
	}
	for _, l := range spec.Labels {
		args = append(args, "--labels="+l)
	}
	for _, rv := range spec.Reviewers {
		args = append(args, "--reviewers="+rv)
	}
	if spec.Draft {
		args = append(args, "--draft")
	}
	if spec.DryRun {
		args = append(args, "--dry-run")
	}
	return args
}

// writeScript wraps the inline command in a POSIX script file, because
// multi-gitter takes a script *path* (which it runs in each repo's checkout),
// not an inline command with arguments.
func writeScript(cmd []string) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "goldfinger-apply-*.sh")
	if err != nil {
		return "", nil, fmt.Errorf("create script: %w", err)
	}
	quoted := make([]string, len(cmd))
	for i, a := range cmd {
		quoted[i] = shellQuote(a)
	}
	content := "#!/bin/sh\nexec " + strings.Join(quoted, " ") + "\n"
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", nil, fmt.Errorf("write script: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", nil, fmt.Errorf("close script: %w", err)
	}
	if err := os.Chmod(f.Name(), 0o755); err != nil {
		os.Remove(f.Name())
		return "", nil, fmt.Errorf("chmod script: %w", err)
	}
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}

// shellQuote single-quotes a token so it survives POSIX sh word-splitting.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
