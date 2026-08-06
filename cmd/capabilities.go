package main

import (
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// capabilitiesVersion is the schema version of the `guide --json` catalogue. It
// is the CLI-surface analogue of the payload versions in jsonout.go: bump it only
// when the catalogue's shape changes incompatibly.
const capabilitiesVersion = 1

// capabilities is the machine-consumable description of goldfinger's CLI surface
// emitted by `guide --json`. It exists because the prose guide is written for
// agents, and an agent parses structure far more reliably than prose (issue #30).
//
// Two provenances are deliberately mixed and must be understood as such:
//   - command names, flag names, and usage text are DERIVED from the live cobra
//     command tree, so they can never drift from the real CLI.
//   - requiredness, enum values, conditional rules, and the per-command example
//     are CURATED (see curatedCapabilities), because cobra does not encode them:
//     requiredness lives in the validation layer (validateApply / validateSign /
//     validateTargeting), not in flag metadata. A test suite keeps the curated
//     set in sync with the validators so the catalogue can't claim a requiredness
//     the code doesn't enforce, or vice-versa.
type capabilities struct {
	Version  int                 `json:"version"`
	Commands []commandCapability `json:"commands"`
}

// commandCapability describes one subcommand.
type commandCapability struct {
	Name          string           `json:"name"`
	Summary       string           `json:"summary"`
	RequiredFlags []string         `json:"requiredFlags"`
	Flags         []flagCapability `json:"flags"`
	Example       string           `json:"example,omitempty"`
	Notes         []string         `json:"notes,omitempty"`
}

// flagCapability describes one flag. Name and Usage are cobra-derived; Required
// and Values are curated (see the type doc on capabilities).
type flagCapability struct {
	Name     string   `json:"name"`
	Usage    string   `json:"usage"`
	Required bool     `json:"required"`
	Values   []string `json:"values,omitempty"`  // allowed enum values, when the flag is an enum
	Default  string   `json:"default,omitempty"` // cobra default, shown only when it is a meaningful non-zero value
}

// curatedCommand is the hand-authored metadata for one command that cobra does
// not encode. It is authored alongside the validation layer and kept in sync by
// the capabilities tests.
type curatedCommand struct {
	requiredFlags []string            // unconditional required flags (names, with the -- prefix)
	enumValues    map[string][]string // flag name -> allowed values
	example       string              // one canonical, runnable invocation
	notes         []string            // conditional-requirement / mutual-exclusion rules cobra can't express
}

// nameSelectionExclusiveNote advertises the --name / --selection mutual exclusion
// that resolveSelectionPath enforces (cmd/selection.go). Every command that binds
// both flags via addSelectionFlags carries it, so the machine catalogue states the
// rule wherever it applies — a test asserts that presence-of-both-flags implies the
// note, so a command can't expose the pair without advertising the conflict.
const nameSelectionExclusiveNote = "--name and --selection are mutually exclusive: pass a named selection or an explicit --selection path, not both"

// nameShapeNote advertises the safe-name shape validSelectionName enforces
// (cmd/selection.go): --name maps to a file in the selections registry, so it must
// be a single plain segment. Carried by every command that binds --name, with a
// guard-tied sync test (TestValidatorBackedNotesStayInSync).
const nameShapeNote = "--name must be a simple registry name: no path separators (/ or \\), and not '.' or '..' (it maps to a file in the selections registry)"

// purposeShapeNote advertises the directory-alphabet validatePurpose enforces
// (cmd/mirror.go): --purpose becomes a directory under ~/goldfinger, so it must be
// a single safe path segment. Guard-tied by the same sync test.
const purposeShapeNote = "--purpose must be a single safe directory-name segment: only letters, digits, and - _ . (no path separators and no '..')"

// curatedCapabilities holds the per-command curated metadata. Every registered
// command has an entry (enforced by a test), and every flag/enum named here must
// exist on the real command and match what the validators enforce (also tested).
var curatedCapabilities = map[string]curatedCommand{
	"select": {
		requiredFlags: []string{"--org"},
		example:       "goldfinger select --org myorg --topic platform",
		notes: []string{
			"exactly one of --all-repos or --topic is required (they are mutually exclusive)",
			"--branch-presence <b> records, read-only, which repos have branch <b> so a later `mirror --branch <b>` can report fall-backs",
			nameSelectionExclusiveNote,
			nameShapeNote,
		},
	},
	"mirror": {
		example: "goldfinger mirror --purpose audit",
		notes: []string{
			"--branch cannot be combined with --clone-depth > 0 (a shallow clone only fetches the default branch)",
			"--workspace and --purpose are mutually exclusive",
			"stdout is the bare workspace path, or the JSON report with --report-json; banners and the reconciliation line go to stderr",
			nameSelectionExclusiveNote,
			nameShapeNote,
			purposeShapeNote,
		},
	},
	"apply": {
		requiredFlags: []string{"--branch", "--commit-message", "--pr-title", "--sign"},
		enumValues:    map[string][]string{"--sign": validSignModes},
		example:       `goldfinger apply --branch bump-dep --commit-message "bump dep" --pr-title "Bump dep" --sign local -- sed -i 's/old/new/' go.mod`,
		notes: []string{
			"a script command is required after -- (e.g. -- sed -i ...)",
			"apply defaults to a dry-run; a real run additionally requires --dry-run=false AND --confirm",
			"--pr-body and --pr-body-file are mutually exclusive",
			nameSelectionExclusiveNote,
			nameShapeNote,
		},
	},
	"check": {
		example: "goldfinger check --json",
		notes: []string{
			"exits non-zero (1) when the selection has drifted from live discovery",
			nameSelectionExclusiveNote,
			nameShapeNote,
		},
	},
	"selections": {
		example: "goldfinger selections --json",
	},
	"doctor": {
		example: "goldfinger doctor",
		notes:   []string{"exits non-zero (1) when a check fails; read-only, opens no PRs and runs no git"},
	},
	"guide": {
		example: "goldfinger guide --json",
	},
	"schema": {
		example: "goldfinger schema",
		notes:   []string{"prints JSON Schema for the lockfile and every machine-readable payload; read-only and offline, needs no token, opens no network, runs no git"},
	},
	"workspaces": {
		example: "goldfinger workspaces list",
		notes: []string{
			"takes one positional action: `list` (enumerate snapshots) or `prune` (remove them)",
			"prune previews by default and deletes only with --confirm (apply's confirm posture); --older-than and --purpose narrow what it targets",
			"--purpose matches only manifest-tagged snapshots whose recorded purpose is exactly that name; it never matches a snapshot without a goldfinger-workspace.json manifest (an unfiltered prune or --older-than still targets a manifest-less snapshot)",
			"acts on snapshot dirs under the workspace root (default ~/goldfinger, override with --root) whose name ends in a -<timestamp> stamp; never touches GitHub and runs no git",
		},
	},
}

// buildCapabilities walks the root command tree and merges each command's
// cobra-derived surface with its curated metadata into the catalogue. The
// command list is sorted by name so the JSON output is deterministic.
func buildCapabilities(root *cobra.Command) capabilities {
	caps := capabilities{Version: capabilitiesVersion}
	for _, cmd := range root.Commands() {
		if isHiddenOrBuiltin(cmd) {
			continue
		}
		caps.Commands = append(caps.Commands, buildCommandCapability(cmd))
	}
	sort.Slice(caps.Commands, func(i, j int) bool { return caps.Commands[i].Name < caps.Commands[j].Name })
	return caps
}

// buildCommandCapability assembles one command's entry: names/usage from cobra,
// requiredness/enums/example/notes from the curated metadata.
func buildCommandCapability(cmd *cobra.Command) commandCapability {
	cur := curatedCapabilities[cmd.Name()]
	required := make(map[string]bool, len(cur.requiredFlags))
	for _, f := range cur.requiredFlags {
		required[f] = true
	}

	cc := commandCapability{
		Name:    cmd.Name(),
		Summary: cmd.Short,
		Example: cur.example,
		Notes:   cur.notes,
	}
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if f.Hidden || f.Name == "help" {
			return
		}
		name := "--" + f.Name
		cc.Flags = append(cc.Flags, flagCapability{
			Name:     name,
			Usage:    f.Usage,
			Required: required[name],
			Values:   cur.enumValues[name],
			Default:  meaningfulDefault(f),
		})
	})
	// RequiredFlags mirrors the curated order, so the headline contract reads in
	// the sequence an operator supplies it.
	cc.RequiredFlags = append([]string{}, cur.requiredFlags...)
	return cc
}

// isHiddenOrBuiltin reports whether a command should be omitted from the
// catalogue: cobra's auto-generated help/completion commands and any hidden one
// are not part of goldfinger's own surface.
func isHiddenOrBuiltin(cmd *cobra.Command) bool {
	if cmd.Hidden {
		return true
	}
	switch cmd.Name() {
	case "help", "completion":
		return true
	}
	return false
}

// meaningfulDefault returns a flag's cobra default only when it is a non-zero
// value worth surfacing (e.g. apply's --dry-run=true, a safety-relevant default).
// Zero-ish defaults are omitted to keep each flag terse.
func meaningfulDefault(f *pflag.Flag) string {
	switch strings.TrimSpace(f.DefValue) {
	case "", "false", "0", "[]", "0s":
		return ""
	}
	return f.DefValue
}
