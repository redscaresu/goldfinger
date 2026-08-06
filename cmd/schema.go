package main

import (
	"github.com/redscaresu/goldfinger/models"
	"github.com/spf13/cobra"
)

// jsonSchemaDialect is the JSON Schema dialect every emitted schema declares. The
// schemas are hand-authored (no code-generation dependency) but pinned to a golden
// file and cross-checked against the Go structs by a reflection test, so they
// cannot silently drift from the types they describe.
const jsonSchemaDialect = "https://json-schema.org/draft/2020-12/schema"

// schemaCatalogue is the payload emitted by `goldfinger schema`: a versioned map
// from surface name to its JSON Schema. It exists so an agent (or the MCP layer)
// can validate goldfinger's machine output without scraping the prose docs — the
// companion to `guide --json`, which describes the *input* surface (issue #27 §4).
//
// The keys mirror the machine surfaces: "lockfile" is the on-disk selection
// (models.Selection); the rest are the --json/report payloads. Each value is a
// self-contained draft-2020-12 schema carrying its own `$schema`, so a consumer can
// hand one straight to a validator.
type schemaCatalogue struct {
	Version int            `json:"version"`
	Schemas map[string]any `json:"schemas"`
}

func newSchemaCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "schema",
		Short: "Print JSON Schema for the lockfile and every machine-readable payload",
		Long: "schema prints the JSON Schema for goldfinger's machine surfaces: the " +
			"selection lockfile plus each command's --json/report payload. It is the " +
			"output-side companion to `guide --json` (which describes the input " +
			"surface), so an agent can validate what goldfinger emits without parsing " +
			"the prose docs.\n\n" +
			"It is entirely read-only and offline: it needs no token, opens no network " +
			"connection, and runs no git. Output is always JSON on stdout; the --json " +
			"flag is accepted for symmetry with the other commands and changes nothing.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return emitJSON(cmd.OutOrStdout(), buildSchemaCatalogue())
		},
	}
	// schema is JSON-only by nature; accept --json as a no-op so an agent can pass it
	// uniformly alongside the other machine commands rather than special-casing this one.
	cmd.Flags().BoolVar(&asJSON, "json", false,
		"accepted for symmetry with the other commands; schema always emits JSON on stdout regardless")
	return cmd
}

// buildSchemaCatalogue assembles the versioned catalogue. It is pure — no I/O, no
// cobra tree — so the exact emitted shape is trivially testable and pinned by a
// golden file. Each named surface is decorated with its dialect/title/description;
// the nested object builders it calls stay bare so they can be reused inline.
func buildSchemaCatalogue() schemaCatalogue {
	return schemaCatalogue{
		Version: schemaCatalogueVersion,
		Schemas: map[string]any{
			"lockfile": schemaDoc("Selection lockfile",
				"The frozen selection persisted to goldfinger.selection and consumed by mirror and apply.",
				selectionSchemaObj()),
			"select": schemaDoc("select --json",
				"The wrapper select --json emits: the on-disk path plus the full lockfile.",
				selectReportSchemaObj()),
			"check": schemaDoc("check --json",
				"The drift report check --json emits against live discovery.",
				checkReportSchemaObj()),
			"selections": schemaDoc("selections --json",
				"The versioned registry listing selections --json emits.",
				selectionsReportSchemaObj()),
			"doctor": schemaDoc("doctor --json",
				"The preflight-check report doctor --json emits.",
				doctorReportSchemaObj()),
			"apply-plan": schemaDoc("apply --plan-json",
				"The invocation plan apply --plan-json emits — what goldfinger is about to run, not the resulting diff.",
				applyPlanSchemaObj()),
			"mirror-report": schemaDoc("mirror --report-json",
				"The mirror summary mirror --report-json emits, built from the lockfile alone.",
				mirrorReportSchemaObj()),
			"capabilities": schemaDoc("guide --json",
				"The CLI-surface catalogue guide --json emits.",
				capabilitiesSchemaObj()),
		},
	}
}

// schemaDoc decorates a bare object schema with the top-level metadata a consumer
// needs to validate against it standalone. It mutates and returns obj, which is
// always a freshly built map from a *SchemaObj builder.
func schemaDoc(title, description string, obj map[string]any) map[string]any {
	obj["$schema"] = jsonSchemaDialect
	obj["title"] = title
	obj["description"] = description
	return obj
}

// --- JSON Schema construction helpers -------------------------------------
//
// These build draft-2020-12 fragments as map[string]any. Objects are closed
// (additionalProperties:false) because every payload has a fixed field set; the one
// open shape is a string->value map, expressed with mapOf. required lists are
// authored in struct-declaration order; a reflection test proves they equal each
// struct's actual always-present (non-omitempty) fields.

func object(props map[string]any, required ...string) map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           props,
		"required":             required,
	}
}

func str() map[string]any     { return map[string]any{"type": "string"} }
func boolean() map[string]any { return map[string]any{"type": "boolean"} }
func integer() map[string]any { return map[string]any{"type": "integer"} }

// dateTime is an RFC 3339 timestamp (Go's time.Time default marshalling).
func dateTime() map[string]any { return map[string]any{"type": "string", "format": "date-time"} }

func enumStr(vals ...string) map[string]any {
	anyVals := make([]any, len(vals))
	for i, v := range vals {
		anyVals[i] = v
	}
	return map[string]any{"type": "string", "enum": anyVals}
}

func arrayOf(items map[string]any) map[string]any {
	return map[string]any{"type": "array", "items": items}
}

// mapOf is a string-keyed object with a uniform value schema and no fixed
// properties — the one open shape (e.g. Repo.branchPresence).
func mapOf(value map[string]any) map[string]any {
	return map[string]any{"type": "object", "additionalProperties": value}
}

// nullable widens a scalar/object schema's `type` to also permit null, for Go
// pointer fields that are always present in the JSON but may be null (a nil
// *ownerTypeFlipJSON, *int, or *string serialises as null, key present).
func nullable(schema map[string]any) map[string]any {
	if t, ok := schema["type"].(string); ok {
		schema["type"] = []any{t, "null"}
	}
	return schema
}

// --- Per-type object schemas ----------------------------------------------
//
// One builder per Go struct that appears in a machine surface. Each is paired with
// its struct in schema_test.go's structural table, which reflects over the struct's
// json tags to prove the schema's properties and required set stay in sync.

func selectionSchemaObj() map[string]any {
	return object(map[string]any{
		"version":         integer(),
		"owner":           str(),
		"ownerType":       enumStr(models.OwnerUser, models.OwnerOrganization),
		"filter":          filterSchemaObj(),
		"resolvedAt":      dateTime(),
		"tool":            str(),
		"repos":           arrayOf(repoSchemaObj()),
		"branchesChecked": arrayOf(str()),
	}, "version", "owner", "ownerType", "filter", "resolvedAt", "tool", "repos")
}

func filterSchemaObj() map[string]any {
	return object(map[string]any{
		"allRepos": boolean(),
		"topics":   arrayOf(str()),
	}, "allRepos")
}

func repoSchemaObj() map[string]any {
	return object(map[string]any{
		"owner":          str(),
		"name":           str(),
		"cloneURL":       str(),
		"defaultBranch":  str(),
		"topics":         arrayOf(str()),
		"archived":       boolean(),
		"branchPresence": mapOf(boolean()),
	}, "owner", "name", "cloneURL", "defaultBranch")
}

func selectReportSchemaObj() map[string]any {
	// No top-level version: select --json nests the full lockfile, whose own
	// `version` is the payload version (documented exception, issue #27 §4).
	return object(map[string]any{
		"selectionPath": str(),
		"selection":     selectionSchemaObj(),
	}, "selectionPath", "selection")
}

func checkReportSchemaObj() map[string]any {
	return object(map[string]any{
		"version":            integer(),
		"name":               str(),
		"inSync":             boolean(),
		"added":              arrayOf(str()),
		"removed":            arrayOf(removedSchemaObj()),
		"defaultBranchMoved": arrayOf(branchMovedSchemaObj()),
		"ownerTypeFlipped":   nullable(ownerFlipSchemaObj()),
	}, "version", "inSync", "added", "removed", "defaultBranchMoved", "ownerTypeFlipped")
}

func removedSchemaObj() map[string]any {
	return object(map[string]any{
		"repo":   str(),
		"reason": str(),
	}, "repo", "reason")
}

func branchMovedSchemaObj() map[string]any {
	return object(map[string]any{
		"repo": str(),
		"from": str(),
		"to":   str(),
	}, "repo", "from", "to")
}

func ownerFlipSchemaObj() map[string]any {
	return object(map[string]any{
		"from": str(),
		"to":   str(),
	}, "from", "to")
}

func selectionsReportSchemaObj() map[string]any {
	return object(map[string]any{
		"version":    integer(),
		"selections": arrayOf(selectionEntrySchemaObj()),
	}, "version", "selections")
}

func selectionEntrySchemaObj() map[string]any {
	return object(map[string]any{
		"name":       str(),
		"path":       str(),
		"owner":      str(),
		"repoCount":  nullable(integer()),
		"resolvedAt": dateTime(),
		"error":      str(),
	}, "name", "path", "repoCount")
}

func doctorReportSchemaObj() map[string]any {
	return object(map[string]any{
		"version": integer(),
		"checks":  arrayOf(doctorCheckSchemaObj()),
	}, "version", "checks")
}

func doctorCheckSchemaObj() map[string]any {
	return object(map[string]any{
		"check":  str(),
		"status": enumStr(statusOK, statusInfo, statusWarn, statusFail),
		"detail": str(),
		"fix":    str(),
	}, "check", "status", "detail")
}

func applyPlanSchemaObj() map[string]any {
	return object(map[string]any{
		"version":            integer(),
		"dry_run":            boolean(),
		"sign_mode":          enumStr(validSignModes...),
		"branch":             str(),
		"pr_title":           str(),
		"commit_message":     str(),
		"pr_body_present":    boolean(),
		"labels":             arrayOf(str()),
		"reviewers":          arrayOf(str()),
		"draft":              boolean(),
		"batch_size":         nullable(integer()),
		"batch_pause":        nullable(str()),
		"command_program":    str(),
		"command_redacted":   boolean(),
		"base_branch_source": str(),
		"repos":              arrayOf(applyPlanRepoSchemaObj()),
		"repos_total":        integer(),
	}, "version", "dry_run", "sign_mode", "branch", "pr_title", "commit_message",
		"pr_body_present", "labels", "reviewers", "draft", "batch_size", "batch_pause",
		"command_program", "command_redacted", "base_branch_source", "repos", "repos_total")
}

func applyPlanRepoSchemaObj() map[string]any {
	return object(map[string]any{
		"repo":                 str(),
		"base_branch_recorded": str(),
	}, "repo", "base_branch_recorded")
}

func mirrorReportSchemaObj() map[string]any {
	return object(map[string]any{
		"version":         integer(),
		"workspace":       str(),
		"owner":           str(),
		"repoCount":       integer(),
		"branch":          str(),
		"branchFactsNote": str(),
		"repos":           arrayOf(mirrorRepoInfoSchemaObj()),
	}, "version", "workspace", "owner", "repoCount", "repos")
}

func mirrorRepoInfoSchemaObj() map[string]any {
	return object(map[string]any{
		"repo":          str(),
		"defaultBranch": str(),
		"branchStatus":  enumStr(branchStatusHas, branchStatusFallback, branchStatusUnknown, branchStatusDefault),
	}, "repo", "defaultBranch", "branchStatus")
}

func capabilitiesSchemaObj() map[string]any {
	return object(map[string]any{
		"version":  integer(),
		"commands": arrayOf(commandCapSchemaObj()),
	}, "version", "commands")
}

func commandCapSchemaObj() map[string]any {
	return object(map[string]any{
		"name":          str(),
		"summary":       str(),
		"requiredFlags": arrayOf(str()),
		"flags":         arrayOf(flagCapSchemaObj()),
		"example":       str(),
		"notes":         arrayOf(str()),
	}, "name", "summary", "requiredFlags", "flags")
}

func flagCapSchemaObj() map[string]any {
	return object(map[string]any{
		"name":     str(),
		"usage":    str(),
		"required": boolean(),
		"values":   arrayOf(str()),
		"default":  str(),
	}, "name", "usage", "required")
}
