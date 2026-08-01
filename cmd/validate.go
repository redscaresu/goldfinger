package main

import (
	"errors"
	"fmt"
)

// validateToken ensures a PAT was provided via the environment.
func validateToken(token string) error {
	if token == "" {
		return errors.New("GITHUB_TOKEN environment variable is required")
	}
	return nil
}

// validateTargeting enforces the repo-selection rules: an org is required, and
// exactly one of --all-repos or --topic must be given.
func validateTargeting(t targeting) error {
	if t.org == "" {
		return errors.New("--org is required")
	}
	if t.allRepos && len(t.topics) > 0 {
		return errors.New("--all-repos and --topic are mutually exclusive")
	}
	if !t.allRepos && len(t.topics) == 0 {
		return errors.New("one of --all-repos or --topic is required")
	}
	return nil
}

// runValidation is the subset of run flags whose presence is mandatory.
type runValidation struct {
	branch        string
	commitMessage string
	prTitle       string
	script        []string
}

// validateRun enforces the flags required to open PRs.
func validateRun(rv runValidation) error {
	if rv.branch == "" {
		return errors.New("--branch is required")
	}
	if rv.commitMessage == "" {
		return errors.New("--commit-message is required")
	}
	if rv.prTitle == "" {
		return errors.New("--pr-title is required")
	}
	if len(rv.script) == 0 {
		return errors.New("a script command is required after --")
	}
	return nil
}

// validateOutput checks the --output format flag.
func validateOutput(format string) error {
	switch format {
	case "table", "json":
		return nil
	default:
		return fmt.Errorf("--output must be 'table' or 'json', got %q", format)
	}
}
