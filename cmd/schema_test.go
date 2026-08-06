package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/redscaresu/goldfinger/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// update regenerates the golden file when set: `go test ./cmd -run TestSchema -update`.
var update = flag.Bool("update", false, "update golden files")

const schemaGoldenPath = "testdata/schema.golden.json"

// TestSchemaCommandMatchesGolden pins the exact JSON `goldfinger schema` emits, so
// any change to a payload's shape must be a deliberate, reviewed golden update
// rather than a silent drift. The catalogue is fully static (no cobra tree, no
// clock), so the output is deterministic.
func TestSchemaCommandMatchesGolden(t *testing.T) {
	out, err := executeCmd(t, "", "schema")
	require.NoError(t, err)

	if *update {
		require.NoError(t, os.WriteFile(schemaGoldenPath, []byte(out), 0o644))
	}
	want, err := os.ReadFile(schemaGoldenPath)
	require.NoError(t, err, "run `go test ./cmd -run TestSchema -update` to generate the golden file")
	assert.Equal(t, string(want), out)
}

// TestSchemaIsValidJSON guards the invariant that schema always emits parseable
// JSON on stdout and nothing on stderr — the stdout=data contract (issue #27 §2).
func TestSchemaIsValidJSON(t *testing.T) {
	root := newRootCmd()
	var stdout, stderr strings.Builder
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"schema"})
	require.NoError(t, root.Execute())

	assert.Empty(t, stderr.String(), "schema must write only to stdout")
	var cat schemaCatalogue
	require.NoError(t, json.Unmarshal([]byte(stdout.String()), &cat))
	assert.Equal(t, schemaCatalogueVersion, cat.Version)
	assert.NotEmpty(t, cat.Schemas)
}

// TestJSONFlagIsANoOp proves --json changes nothing: schema is JSON-only and only
// accepts the flag for uniformity with the other machine commands.
func TestJSONFlagIsANoOp(t *testing.T) {
	plain, err := executeCmd(t, "", "schema")
	require.NoError(t, err)
	withFlag, err := executeCmd(t, "", "schema", "--json")
	require.NoError(t, err)
	assert.Equal(t, plain, withFlag)
}

// TestSchemasMatchTheirStructs is the anti-drift core: for every Go struct that
// appears in a machine surface, it reflects over the struct's json tags and proves
// the hand-authored schema's `properties` covers exactly those fields, and its
// `required` list equals the struct's always-present (non-omitempty) fields.
//
// required is derived behaviourally — by marshalling the struct's zero value and
// taking the keys that survive — so it is tied to what the code actually always
// emits, not to a second hand-authored list that could itself drift.
func TestSchemasMatchTheirStructs(t *testing.T) {
	cases := []struct {
		name   string
		typ    reflect.Type
		schema map[string]any
	}{
		{"Selection", reflect.TypeOf(models.Selection{}), selectionSchemaObj()},
		{"SelectionFilter", reflect.TypeOf(models.SelectionFilter{}), filterSchemaObj()},
		{"Repo", reflect.TypeOf(models.Repo{}), repoSchemaObj()},
		{"selectJSONReport", reflect.TypeOf(selectJSONReport{}), selectReportSchemaObj()},
		{"checkReport", reflect.TypeOf(checkReport{}), checkReportSchemaObj()},
		{"removedJSON", reflect.TypeOf(removedJSON{}), removedSchemaObj()},
		{"branchMovedJSON", reflect.TypeOf(branchMovedJSON{}), branchMovedSchemaObj()},
		{"ownerTypeFlipJSON", reflect.TypeOf(ownerTypeFlipJSON{}), ownerFlipSchemaObj()},
		{"selectionsReport", reflect.TypeOf(selectionsReport{}), selectionsReportSchemaObj()},
		{"selectionEntryJSON", reflect.TypeOf(selectionEntryJSON{}), selectionEntrySchemaObj()},
		{"doctorReport", reflect.TypeOf(doctorReport{}), doctorReportSchemaObj()},
		{"doctorCheck", reflect.TypeOf(doctorCheck{}), doctorCheckSchemaObj()},
		{"applyPlan", reflect.TypeOf(applyPlan{}), applyPlanSchemaObj()},
		{"applyPlanRepo", reflect.TypeOf(applyPlanRepo{}), applyPlanRepoSchemaObj()},
		{"mirrorReport", reflect.TypeOf(mirrorReport{}), mirrorReportSchemaObj()},
		{"mirrorRepoInfo", reflect.TypeOf(mirrorRepoInfo{}), mirrorRepoInfoSchemaObj()},
		{"capabilities", reflect.TypeOf(capabilities{}), capabilitiesSchemaObj()},
		{"commandCapability", reflect.TypeOf(commandCapability{}), commandCapSchemaObj()},
		{"flagCapability", reflect.TypeOf(flagCapability{}), flagCapSchemaObj()},
		{"workspacesReport", reflect.TypeOf(workspacesReport{}), workspacesReportSchemaObj()},
		{"workspaceInfo", reflect.TypeOf(workspaceInfo{}), workspaceInfoSchemaObj()},
		{"workspaceManifest", reflect.TypeOf(workspaceManifest{}), workspaceManifestSchemaObj()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			props, ok := tc.schema["properties"].(map[string]any)
			require.True(t, ok, "schema must have a properties object")
			propKeys := make([]string, 0, len(props))
			for k := range props {
				propKeys = append(propKeys, k)
			}
			assert.ElementsMatch(t, jsonFieldNames(tc.typ), propKeys,
				"properties must cover exactly the struct's json fields")

			required, ok := tc.schema["required"].([]string)
			require.True(t, ok, "schema must have a required list")
			assert.ElementsMatch(t, requiredFromZeroValue(t, tc.typ), required,
				"required must equal the struct's always-present (non-omitempty) fields")
		})
	}
}

// TestCatalogueUsesOnlyDeclaredSchemas guards the shape of every named surface: it
// walks the emitted catalogue and asserts each is a closed object schema declaring
// the dialect. Field-level accuracy is proven per-struct by TestSchemasMatchTheirStructs.
func TestCatalogueUsesOnlyDeclaredSchemas(t *testing.T) {
	cat := buildSchemaCatalogue()
	// Every named surface must be a closed object schema so nothing is silently
	// under-specified; the per-struct test proves the field-level accuracy.
	for name, s := range cat.Schemas {
		obj, ok := s.(map[string]any)
		require.Truef(t, ok, "schema %q must be an object", name)
		assert.Equalf(t, "object", obj["type"], "schema %q must be an object type", name)
		assert.Equalf(t, false, obj["additionalProperties"], "schema %q must be closed", name)
		assert.Equalf(t, jsonSchemaDialect, obj["$schema"], "schema %q must declare its dialect", name)
	}
}

// TestSampleOutputValidatesAgainstSchema closes the gap the name/required checks
// leave open: TestSchemasMatchTheirStructs proves the property *set* and required
// list, but not the declared *types*, enum values, nullable widening, or nested
// item schemas — a field flipped from *int to *string (same json tag, same
// omitempty) would slip past it. Here a realistically populated instance of each
// surface is marshalled the way the command emits it and validated against the
// schema: types, enum membership, additionalProperties:false, and item schemas all
// checked. No JSON-Schema library is pulled in (no new dep) — validateAgainstSchema
// is a focused checker for the draft-2020-12 subset the catalogue uses.
func TestSampleOutputValidatesAgainstSchema(t *testing.T) {
	n := 3
	pause := "30s"
	sel := models.Selection{
		Version:    models.SelectionVersion,
		Owner:      "acme",
		OwnerType:  models.OwnerOrganization,
		Filter:     models.SelectionFilter{AllRepos: false, Topics: []string{"platform"}},
		ResolvedAt: time.Now().UTC(),
		Tool:       "goldfinger test",
		Repos: []models.Repo{{
			Owner: "acme", Name: "a", CloneURL: "https://github.com/acme/a.git",
			DefaultBranch: "main", Topics: []string{"platform"}, Archived: false,
			BranchPresence: map[string]bool{"dev": true},
		}},
		BranchesChecked: []string{"dev"},
	}
	samples := map[string]any{
		"lockfile": sel,
		"select":   selectJSONReport{SelectionPath: "goldfinger.selection", Selection: sel},
		"check": checkReport{
			Version: checkReportVersion, Name: "platform", InSync: false,
			Added:              []string{"acme/new"},
			Removed:            []removedJSON{{Repo: "acme/old", Reason: "archived"}},
			DefaultBranchMoved: []branchMovedJSON{{Repo: "acme/a", From: "master", To: "main"}},
			OwnerTypeFlipped:   &ownerTypeFlipJSON{From: "User", To: "Organization"},
		},
		"selections": selectionsReport{
			Version: selectionsReportVersion,
			Selections: []selectionEntryJSON{{
				Name: "platform", Path: "/p", Owner: "acme",
				RepoCount: &n, ResolvedAt: time.Now().UTC().Format(time.RFC3339),
			}},
		},
		"doctor": doctorReport{
			Version: doctorReportVersion,
			Checks:  []doctorCheck{{Check: "auth", Status: statusOK, Detail: "ok", Fix: "do the thing"}},
		},
		"apply-plan": applyPlan{
			Version: applyPlanVersion, DryRun: true, SignMode: models.SignLocal,
			Branch: "b", PRTitle: "t", CommitMessage: "m", PRBodyPresent: true,
			Labels: []string{"x"}, Reviewers: []string{"y"}, Draft: true,
			BatchSize: &n, BatchPause: &pause, CommandProgram: "sed", CommandRedacted: true,
			BaseBranchSrc: "per-repo-default",
			Repos:         []applyPlanRepo{{Repo: "acme/a", BaseBranchRecorded: "main"}},
			ReposTotal:    1,
		},
		"mirror-report": mirrorReport{
			Version: mirrorReportVersion, Workspace: "/ws", Owner: "acme",
			RepoCount: 1, Branch: "dev", BranchFactsNote: branchFactsNote,
			Repos: []mirrorRepoInfo{{Repo: "acme/a", DefaultBranch: "main", BranchStatus: branchStatusHas}},
		},
		"capabilities": buildCapabilities(newRootCmd()),
		"workspaces": workspacesReport{
			Version: workspacesReportVersion, Root: "/home/u/goldfinger",
			Action: workspaceActionList, Pruned: false,
			Workspaces: []workspaceInfo{{
				Path: "/home/u/goldfinger/audit-dev-2026-08-05-101112.131", Purpose: "audit",
				Branch: "dev", Stamp: "2026-08-05-101112.131", Owner: "acme", SizeBytes: 4096,
				CreatedAt: time.Now().UTC().Format(time.RFC3339), ManifestPresent: true,
			}},
		},
		"workspace-manifest": workspaceManifest{
			Version: workspaceManifestVersion, Purpose: "audit", Branch: "dev",
			Stamp: "2026-08-05-101112.131", Owner: "acme", CreatedAt: time.Now().UTC(),
		},
	}

	cat := buildSchemaCatalogue()
	require.Equal(t, len(cat.Schemas), len(samples), "every catalogue surface needs a sample")
	for name, schema := range cat.Schemas {
		sample, ok := samples[name]
		require.Truef(t, ok, "no sample for catalogue surface %q", name)
		validateSampleAgainstSchema(t, name, sample, schema.(map[string]any))
	}

	// Null variants exercise the "null" arm of every nullable field, which the
	// populated samples above (pointers all non-nil) never reach: without these, a
	// field that lost its `"null"` type would still validate. Each nulls exactly the
	// pointer fields of one surface — a nil *ownerTypeFlipJSON, *int (repoCount /
	// batch_size), and *string (batch_pause) all serialise as JSON null.
	nullVariants := []struct {
		key    string
		sample any
	}{
		{"check", checkReport{
			Version: checkReportVersion, InSync: true,
			Added: []string{}, Removed: []removedJSON{}, DefaultBranchMoved: []branchMovedJSON{},
			OwnerTypeFlipped: nil,
		}},
		{"selections", selectionsReport{
			Version:    selectionsReportVersion,
			Selections: []selectionEntryJSON{{Name: "broken", Path: "/p", RepoCount: nil, Error: "unreadable"}},
		}},
		{"apply-plan", applyPlan{
			Version: applyPlanVersion, DryRun: true, SignMode: models.SignNone,
			Branch: "b", PRTitle: "t", CommitMessage: "m", PRBodyPresent: false,
			Labels: []string{}, Reviewers: []string{}, Draft: false,
			BatchSize: nil, BatchPause: nil, CommandProgram: "sed", CommandRedacted: true,
			BaseBranchSrc: "per-repo-default",
			Repos:         []applyPlanRepo{}, ReposTotal: 0,
		}},
		// A manifest-less snapshot: the omitempty purpose/branch/owner fields drop
		// out entirely, exercising that the schema marks them optional (only
		// path/sizeBytes/manifestPresent are required).
		{"workspaces", workspacesReport{
			Version: workspacesReportVersion, Root: "/home/u/goldfinger",
			Action: workspaceActionPrune, Pruned: false,
			Workspaces: []workspaceInfo{{
				Path: "/home/u/goldfinger/legacy-2026-01-02-030405.006",
				Stamp: "2026-01-02-030405.006", SizeBytes: 0, ManifestPresent: false,
			}},
		}},
	}
	for _, nv := range nullVariants {
		schema, ok := cat.Schemas[nv.key].(map[string]any)
		require.Truef(t, ok, "no schema for %q", nv.key)
		validateSampleAgainstSchema(t, nv.key+" (null variant)", nv.sample, schema)
	}
}

// validateSampleAgainstSchema marshals a sample the way the command emits it and
// validates the resulting JSON against schema.
func validateSampleAgainstSchema(t *testing.T, label string, sample any, schema map[string]any) {
	t.Helper()
	raw, err := json.Marshal(sample)
	require.NoError(t, err)
	var decoded any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	validateAgainstSchema(t, label, decoded, schema)
}

// validateAgainstSchema asserts a JSON value (decoded into any) conforms to schema:
// the declared type(s), enum membership, required keys, closed object shape
// (additionalProperties:false ⇒ no undeclared keys), and — recursively — object
// properties, map values, and array items. It covers only the draft-2020-12 subset
// the catalogue emits, deliberately, so the test needs no schema-validator dep.
func validateAgainstSchema(t *testing.T, path string, v any, schema map[string]any) {
	t.Helper()
	types := schemaTypes(schema)
	actual := jsonKind(v)
	require.Truef(t, typeAllowed(actual, types), "%s: value kind %q not permitted by schema types %v", path, actual, types)

	if enum, ok := schema["enum"].([]any); ok {
		require.Containsf(t, enum, v, "%s: value %v is not in the schema enum %v", path, v, enum)
	}

	switch actual {
	case "object":
		m := v.(map[string]any)
		if props, ok := schema["properties"].(map[string]any); ok {
			for _, r := range schemaRequired(schema) {
				_, present := m[r]
				assert.Truef(t, present, "%s: required key %q missing from sample output", path, r)
			}
			for k, val := range m {
				ps, ok := props[k].(map[string]any)
				require.Truef(t, ok, "%s: key %q is in the output but not declared in schema properties (additionalProperties:false)", path, k)
				validateAgainstSchema(t, path+"."+k, val, ps)
			}
			return
		}
		if ap, ok := schema["additionalProperties"].(map[string]any); ok {
			for k, val := range m {
				validateAgainstSchema(t, fmt.Sprintf("%s[%q]", path, k), val, ap)
			}
		}
	case "array":
		if items, ok := schema["items"].(map[string]any); ok {
			for i, el := range v.([]any) {
				validateAgainstSchema(t, fmt.Sprintf("%s[%d]", path, i), el, items)
			}
		}
	}
}

// schemaTypes returns a schema's declared type(s), whether authored as a single
// string ("string") or a nullable pair (["integer","null"]).
func schemaTypes(schema map[string]any) []string {
	switch tv := schema["type"].(type) {
	case string:
		return []string{tv}
	case []any:
		out := make([]string, 0, len(tv))
		for _, x := range tv {
			out = append(out, x.(string))
		}
		return out
	default:
		return nil
	}
}

// schemaRequired returns a schema's required-field list (authored as []string).
func schemaRequired(schema map[string]any) []string {
	r, _ := schema["required"].([]string)
	return r
}

// jsonKind classifies a value decoded from JSON into any. JSON has one number type,
// so an integral float64 is reported as "integer" and a fractional one as "number".
func jsonKind(v any) string {
	switch num := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case float64:
		if num == math.Trunc(num) {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// typeAllowed reports whether actual is one of the schema's declared types, treating
// an integer as an acceptable number.
func typeAllowed(actual string, allowed []string) bool {
	for _, a := range allowed {
		if a == actual || (actual == "integer" && a == "number") {
			return true
		}
	}
	return false
}

// TestEveryJSONEmittingCommandHasASchema is the completeness guard, both ways: every
// command exposing a machine-output flag (--json/--report-json/--plan-json) must map
// to a catalogue schema, AND every non-lockfile schema must be claimed by exactly one
// such command. So a new JSON surface can't ship undocumented, an orphaned schema
// can't linger, and a second command can't quietly reuse an existing key. It walks
// the root's subcommands only — the CLI is deliberately flat (asserted elsewhere); if
// nesting is ever added, recurse here. `schema` itself is excluded: its --json is a
// no-op and it describes the *other* surfaces rather than having a payload of its own.
func TestEveryJSONEmittingCommandHasASchema(t *testing.T) {
	// command name -> the catalogue key describing its machine payload.
	surfaceKey := map[string]string{
		"select": "select", "check": "check", "selections": "selections",
		"doctor": "doctor", "mirror": "mirror-report", "apply": "apply-plan",
		"guide": "capabilities", "workspaces": "workspaces",
	}
	cat := buildSchemaCatalogue()
	claimedBy := map[string]int{}
	for _, cmd := range newRootCmd().Commands() {
		if isHiddenOrBuiltin(cmd) || cmd.Name() == "schema" {
			continue
		}
		emitsJSON := false
		for _, fn := range []string{"json", "report-json", "plan-json"} {
			if cmd.Flags().Lookup(fn) != nil {
				emitsJSON = true
				break
			}
		}
		if !emitsJSON {
			continue
		}
		key, ok := surfaceKey[cmd.Name()]
		require.Truef(t, ok, "command %q emits JSON but has no mapped schema surface — add one to surfaceKey and the catalogue", cmd.Name())
		_, ok = cat.Schemas[key]
		assert.Truef(t, ok, "command %q emits JSON but the catalogue has no %q schema", cmd.Name(), key)
		claimedBy[key]++
	}

	// On-disk artifacts have no emitting command — the lockfile is the persisted
	// selection, and workspace-manifest is the sidecar mirror --purpose writes into
	// each snapshot. Every other schema must be claimed by exactly one
	// JSON-emitting command.
	onDiskArtifacts := map[string]bool{"lockfile": true, "workspace-manifest": true}
	for key := range cat.Schemas {
		if onDiskArtifacts[key] {
			continue
		}
		assert.Equalf(t, 1, claimedBy[key], "schema %q must be referenced by exactly one JSON-emitting command, got %d", key, claimedBy[key])
	}
}

// jsonFieldNames returns the json field names on a struct type (the part before any
// comma in the tag), skipping untagged and `-` fields.
func jsonFieldNames(t reflect.Type) []string {
	var names []string
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name == "" {
			continue
		}
		names = append(names, name)
	}
	return names
}

// requiredFromZeroValue marshals a struct's zero value and returns the top-level
// keys that survive — i.e. exactly its non-omitempty fields, since every omitempty
// field is empty at the zero value and drops out.
func requiredFromZeroValue(t *testing.T, typ reflect.Type) []string {
	t.Helper()
	zero := reflect.New(typ).Elem().Interface()
	b, err := json.Marshal(zero)
	require.NoError(t, err)
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(b, &m))
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
