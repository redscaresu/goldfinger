package main

import (
	"errors"
	"fmt"

	"github.com/redscaresu/goldfinger/models"
)

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

// mirrorValidation is the subset of `mirror` flags whose combination needs
// checking before any token/tool/selection work.
type mirrorValidation struct {
	branch     string
	cloneDepth int
}

// validateMirror rejects flag combinations that ghorg cannot honour. A shallow
// clone (--clone-depth > 0) only fetches each repo's default branch, so a
// --branch request would silently fall back to the default rather than checking
// out the asked-for branch — refuse it instead of quietly changing the depth.
func validateMirror(mv mirrorValidation) error {
	if mv.branch != "" && mv.cloneDepth > 0 {
		return errors.New("--branch cannot be combined with --clone-depth > 0: ghorg shallow clones only fetch the default branch; omit --clone-depth or use --clone-depth 0")
	}
	return nil
}

// applyValidation is the subset of `apply` flags whose presence is mandatory.
type applyValidation struct {
	branch        string
	commitMessage string
	prTitle       string
	sign          string
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
	if err := validateSign(av.sign); err != nil {
		return err
	}
	if len(av.script) == 0 {
		return errors.New("a script command is required after --")
	}
	return nil
}

// validateSign enforces that --sign is set to a known mode. There is no default:
// a real run must declare its signing intent explicitly.
func validateSign(mode string) error {
	switch mode {
	case models.SignLocal, models.SignGitHub, models.SignNone:
		return nil
	case "":
		return fmt.Errorf("--sign is required: one of %s (your GPG key), %s (GitHub-verified), or %s (unsigned)", models.SignLocal, models.SignGitHub, models.SignNone)
	default:
		return fmt.Errorf("--sign %q is invalid: must be one of %s, %s, or %s", mode, models.SignLocal, models.SignGitHub, models.SignNone)
	}
}
