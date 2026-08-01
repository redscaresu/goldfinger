package main

import (
	"errors"
	"fmt"
)

// validateToken ensures a PAT was provided via the environment.
func validateToken(token string) error {
	if token == "" {
		return fmt.Errorf("%s environment variable is required", tokenEnvVar)
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

// applyValidation is the subset of `apply` flags whose presence is mandatory.
type applyValidation struct {
	branch        string
	commitMessage string
	prTitle       string
	script        []string
}

// validateApply enforces the flags required to run a change via multi-gitter.
func validateApply(av applyValidation) error {
	if av.branch == "" {
		return errors.New("--branch is required")
	}
	if av.commitMessage == "" {
		return errors.New("--commit-message is required")
	}
	if av.prTitle == "" {
		return errors.New("--pr-title is required")
	}
	if len(av.script) == 0 {
		return errors.New("a script command is required after --")
	}
	return nil
}
