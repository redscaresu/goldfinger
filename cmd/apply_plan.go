package main

import (
	"github.com/redscaresu/goldfinger/models"
)

// applyPlan is the --plan-json payload for apply (issue #27 §3): a machine-readable
// summary of *what goldfinger is about to invoke*, not the resulting diff.
// goldfinger delegates clone/script/diff to multi-gitter and only receives the
// child's exit status, so the plan deliberately carries only the invocation
// metadata goldfinger controls — never per-repo changed flags or a diffstat, which
// it cannot know without reimplementing git.
//
// Two safety choices are baked into the shape:
//   - CommandProgram is argv[0] only, with CommandRedacted=true: the operator's
//     script after `--` is arbitrary and may itself carry secrets, so the full
//     argv is never emitted by default.
//   - the PR body is reduced to PRBodyPresent (a bool): it is the operator's and
//     can be large; presence is enough for an agent to reason about the plan.
type applyPlan struct {
	Version         int             `json:"version"`
	DryRun          bool            `json:"dry_run"`
	SignMode        string          `json:"sign_mode"`
	Branch          string          `json:"branch"`
	PRTitle         string          `json:"pr_title"`
	CommitMessage   string          `json:"commit_message"`
	PRBodyPresent   bool            `json:"pr_body_present"`
	Labels          []string        `json:"labels"`
	Reviewers       []string        `json:"reviewers"`
	Draft           bool            `json:"draft"`
	BatchSize       *int            `json:"batch_size"`
	BatchPause      *string         `json:"batch_pause"`
	CommandProgram  string          `json:"command_program"`
	CommandRedacted bool            `json:"command_redacted"`
	BaseBranchSrc   string          `json:"base_branch_source"`
	Repos           []applyPlanRepo `json:"repos"`
	ReposTotal      int             `json:"repos_total"`
}

// applyPlanRepo is the per-repo slice of the plan. BaseBranchRecorded is the
// lockfile-recorded default (or the explicit --base-branch). When --base-branch is
// omitted goldfinger passes nothing and multi-gitter targets each repo's *live*
// default at run time, which can differ from the recorded value — the field name
// and docs carry that drift caveat; it is not exact per-repo routing.
type applyPlanRepo struct {
	Repo               string `json:"repo"`
	BaseBranchRecorded string `json:"base_branch_recorded"`
}

// buildApplyPlan assembles the plan from the selection and the resolved ApplySpec.
// It is pure — no I/O — so the exact shape is trivially testable and provably free
// of any diff/git introspection.
func buildApplyPlan(sel models.Selection, spec models.ApplySpec) applyPlan {
	plan := applyPlan{
		Version:       applyPlanVersion,
		DryRun:        spec.DryRun,
		SignMode:      spec.Sign,
		Branch:        spec.Branch,
		PRTitle:       spec.PRTitle,
		CommitMessage: spec.CommitMessage,
		PRBodyPresent: spec.PRBody != "",
		// Normalise nil→[] so the machine contract always carries a JSON array for
		// these documented list fields, never null (mirrors select's repos handling).
		Labels:          orEmptyStrings(spec.Labels),
		Reviewers:       orEmptyStrings(spec.Reviewers),
		Draft:           spec.Draft,
		CommandRedacted: true,
		BaseBranchSrc:   baseBranchSource(spec.BaseBranch),
		Repos:           make([]applyPlanRepo, 0, len(sel.Repos)),
		ReposTotal:      len(sel.Repos),
	}
	if len(spec.Script) > 0 {
		plan.CommandProgram = spec.Script[0]
	}
	if spec.BatchSize > 0 {
		n := spec.BatchSize
		plan.BatchSize = &n
	}
	if spec.BatchPause > 0 {
		d := spec.BatchPause.String()
		plan.BatchPause = &d
	}
	for _, r := range sel.Repos {
		plan.Repos = append(plan.Repos, applyPlanRepo{
			Repo:               r.FullName(),
			BaseBranchRecorded: resolveBase(spec.BaseBranch, r),
		})
	}
	return plan
}

// orEmptyStrings returns in, or a non-nil empty slice when in is nil, so a JSON
// list field serialises as [] rather than null.
func orEmptyStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// baseBranchSource labels how the PR base is chosen: an explicit global
// --base-branch, or each repo's own (recorded) default.
func baseBranchSource(globalBase string) string {
	if globalBase != "" {
		return "explicit:" + globalBase
	}
	return "per-repo-default"
}
