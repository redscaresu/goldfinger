package main

import (
	"encoding/json"
	"fmt"
	"io"
)

// Machine-readable payload versions (issue #27 §4). Each JSON surface carries an
// explicit top-level `version` so agents and the MCP layer can branch on shape
// across releases — the one exception is `select --json`, which nests the full
// lockfile and therefore carries the lockfile's own `version` (models.Selection
// Version) rather than a second, parallel one.
//
// These are the *payload* schema versions, distinct from the lockfile schema
// version (models.SelectionVersion). Bump a payload's constant only when its shape
// changes incompatibly.
const (
	checkReportVersion       = 1
	selectionsReportVersion  = 1
	mirrorReportVersion      = 1
	applyPlanVersion         = 1
	doctorReportVersion      = 1
	schemaCatalogueVersion   = 1
	workspacesReportVersion  = 1
	workspaceManifestVersion = 1
)

// emitJSON writes v as JSON followed by a newline to w. It is the single path
// every `--json`/report output goes through, so the stdout=data contract (issue
// #27 §2) is enforced in one place: callers pass cmd.OutOrStdout() and keep all
// human banners on stderr.
//
// compact selects the on-the-wire format without touching the shape: under
// --quiet (machine mode) callers pass compact=true for single-line JSON that
// costs an agent fewer tokens; the default human path passes false for the
// indented form that stays readable in a terminal. Only whitespace differs, so
// the schema golden (which pins the indented rendering) is unaffected.
func emitJSON(w io.Writer, v any, compact bool) error {
	var (
		data []byte
		err  error
	)
	if compact {
		data, err = json.Marshal(v)
	} else {
		data, err = json.MarshalIndent(v, "", "  ")
	}
	if err != nil {
		return fmt.Errorf("render JSON output: %w", err)
	}
	_, err = fmt.Fprintln(w, string(data))
	return err
}
