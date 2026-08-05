package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/redscaresu/goldfinger/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// findCommand returns the catalogue entry for name, or fails the test.
func findCommand(t *testing.T, caps capabilities, name string) commandCapability {
	t.Helper()
	for _, c := range caps.Commands {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("catalogue has no command %q", name)
	return commandCapability{}
}

func findFlag(t *testing.T, cc commandCapability, name string) flagCapability {
	t.Helper()
	for _, f := range cc.Flags {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("command %q has no flag %q in catalogue", cc.Name, name)
	return flagCapability{}
}

func TestGuideJSONEmitsVersionedCatalogue(t *testing.T) {
	// guide --json needs no token and no network; stdout is the JSON alone.
	out, err := executeCmd(t, "", "guide", "--json")
	require.NoError(t, err)

	var caps capabilities
	require.NoError(t, json.Unmarshal([]byte(out), &caps), "guide --json must emit parseable JSON")
	assert.Equal(t, capabilitiesVersion, caps.Version)

	apply := findCommand(t, caps, "apply")
	assert.NotEmpty(t, apply.Summary)
	assert.NotEmpty(t, apply.Example, "each command carries a canonical example")
	assert.ElementsMatch(t, []string{"--branch", "--commit-message", "--pr-title", "--sign"}, apply.RequiredFlags)

	sign := findFlag(t, apply, "--sign")
	assert.True(t, sign.Required)
	assert.ElementsMatch(t, []string{models.SignLocal, models.SignGitHub, models.SignNone}, sign.Values)

	// A safety-relevant cobra default is surfaced.
	assert.Equal(t, "true", findFlag(t, apply, "--dry-run").Default, "apply's dry-run-by-default must be visible in the catalogue")
}

// TestGuideJSONWritesOnlyToStdout independently asserts the JSON contract for
// guide --json: the catalogue is the ONLY thing on stdout and stderr stays empty,
// so an agent can parse stdout without stripping banners. The shared executeCmd
// harness merges the two streams, so this test wires them separately to prove the
// split rather than assume it.
func TestGuideJSONWritesOnlyToStdout(t *testing.T) {
	t.Setenv(tokenEnvVar, "")
	stubGhToken(t, "", false)
	root := newRootCmd()
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs([]string{"guide", "--json"})
	require.NoError(t, root.Execute())

	assert.Empty(t, errBuf.String(), "guide --json must write nothing to stderr")
	var caps capabilities
	require.NoError(t, json.Unmarshal(out.Bytes(), &caps), "stdout must be the catalogue JSON alone")
	assert.Equal(t, capabilitiesVersion, caps.Version)
}

func TestGuidePlainIsStillProseNotJSON(t *testing.T) {
	out, err := executeCmd(t, "", "guide")
	require.NoError(t, err)
	assert.False(t, strings.HasPrefix(strings.TrimSpace(out), "{"), "plain guide must stay prose, not JSON")
}

// TestCapabilitiesListsExactlyTheRegisteredCommands is the completeness guard: the
// catalogue is derived from the cobra tree, so it must contain every registered
// command (minus cobra's built-ins) and no phantom ones.
func TestCapabilitiesListsExactlyTheRegisteredCommands(t *testing.T) {
	root := newRootCmd()
	caps := buildCapabilities(root)

	var registered []string
	for _, cmd := range root.Commands() {
		if isHiddenOrBuiltin(cmd) {
			continue
		}
		registered = append(registered, cmd.Name())
	}
	var catalogued []string
	for _, c := range caps.Commands {
		catalogued = append(catalogued, c.Name)
	}
	assert.ElementsMatch(t, registered, catalogued,
		"the catalogue must list every registered command and nothing extra")

	// A hardcoded anchor, independent of isHiddenOrBuiltin: if that filter ever
	// wrongly drops a real command, the ElementsMatch above would still pass (both
	// sides share the filter), but this fixed set would catch the omission.
	assert.ElementsMatch(t,
		[]string{"select", "mirror", "apply", "check", "selections", "doctor", "guide", "schema"},
		catalogued,
		"the catalogue must list exactly goldfinger's own commands")
}

// TestEveryCommandHasCuratedExample forces a hand-authored example per command, so
// adding a command without one fails rather than shipping a gap.
func TestEveryCommandHasCuratedExample(t *testing.T) {
	caps := buildCapabilities(newRootCmd())
	for _, c := range caps.Commands {
		assert.NotEmptyf(t, c.Example, "command %q needs a curated example", c.Name)
	}
}

// TestCuratedMetadataNamesRealCommandsAndFlags catches curated metadata drifting
// from the real CLI: every curated command must be registered, and every flag
// named in a command's requiredFlags or enum map must actually exist on it.
func TestCuratedMetadataNamesRealCommandsAndFlags(t *testing.T) {
	root := newRootCmd()
	byName := map[string]bool{}
	cmds := map[string]bool{}
	for _, cmd := range root.Commands() {
		cmds[cmd.Name()] = true
	}
	for _, cmd := range root.Commands() {
		byName[cmd.Name()] = true
	}

	for name, cur := range curatedCapabilities {
		require.Truef(t, cmds[name], "curated metadata for unregistered command %q", name)
		cmd, _, err := root.Find([]string{name})
		require.NoError(t, err)

		named := append([]string{}, cur.requiredFlags...)
		for f := range cur.enumValues {
			named = append(named, f)
		}
		for _, flag := range named {
			bare := strings.TrimPrefix(flag, "--")
			assert.NotNilf(t, cmd.Flags().Lookup(bare), "curated flag %q does not exist on command %q", flag, name)
		}
	}
}

// TestEveryRegisteredCommandIsCurated ensures a new command can't slip in without
// curated metadata (example/notes/requiredness), which the catalogue relies on.
func TestEveryRegisteredCommandIsCurated(t *testing.T) {
	root := newRootCmd()
	for _, cmd := range root.Commands() {
		if isHiddenOrBuiltin(cmd) {
			continue
		}
		_, ok := curatedCapabilities[cmd.Name()]
		assert.Truef(t, ok, "registered command %q has no curated capabilities entry", cmd.Name())
	}
}

// TestApplyRequiredFlagsMatchValidator ties the curated "required" claim for apply
// to what validateApply actually enforces: blanking each curated required flag
// must fail validation, and the curated set must be exactly those flags — so the
// catalogue can't claim a requiredness the validator doesn't enforce, or omit one
// it does.
func TestApplyRequiredFlagsMatchValidator(t *testing.T) {
	valid := applyValidation{branch: "b", commitMessage: "m", prTitle: "t", sign: models.SignNone, script: []string{"true"}}
	require.NoError(t, validateApply(valid), "the baseline valid case must pass")

	blankers := map[string]func(applyValidation) applyValidation{
		"--branch":         func(v applyValidation) applyValidation { v.branch = ""; return v },
		"--commit-message": func(v applyValidation) applyValidation { v.commitMessage = ""; return v },
		"--pr-title":       func(v applyValidation) applyValidation { v.prTitle = ""; return v },
		"--sign":           func(v applyValidation) applyValidation { v.sign = ""; return v },
	}
	var enforced []string
	for flag, blank := range blankers {
		assert.Errorf(t, validateApply(blank(valid)), "blanking %s must fail validation", flag)
		enforced = append(enforced, flag)
	}
	assert.ElementsMatch(t, enforced, curatedCapabilities["apply"].requiredFlags,
		"curated apply required flags must be exactly those validateApply enforces")
}

// TestSelectRequiredFlagMatchesValidator ties select's curated --org requirement
// to validateTargeting.
func TestSelectRequiredFlagMatchesValidator(t *testing.T) {
	assert.Equal(t, []string{"--org"}, curatedCapabilities["select"].requiredFlags)
	assert.Error(t, validateTargeting(targeting{allRepos: true}), "missing --org must fail validation")
	assert.NoError(t, validateTargeting(targeting{org: "o", allRepos: true}))
}

// TestSignEnumMatchesValidator ties the curated --sign enum to validateSign, and
// proves the advertised enum is EXHAUSTIVE, not merely a subset: the catalogue
// reads the same validSignModes the validator does, so it can never omit a mode
// the validator accepts. We assert that shared identity, that every value is
// accepted, and that a value outside the set is rejected.
func TestSignEnumMatchesValidator(t *testing.T) {
	vals := curatedCapabilities["apply"].enumValues["--sign"]
	assert.Equal(t, validSignModes, vals, "the catalogue enum must be the same source of truth the validator uses")
	assert.ElementsMatch(t, []string{models.SignLocal, models.SignGitHub, models.SignNone}, vals)
	for _, v := range vals {
		assert.NoErrorf(t, validateSign(v), "advertised --sign value %q must be accepted by the validator", v)
	}
	assert.Error(t, validateSign("bogus"), "a value outside the advertised enum must be rejected")
}

// TestNameSelectionExclusionIsAdvertisedWhereverBothFlagsExist ties the catalogue
// to the resolveSelectionPath rule: any command exposing BOTH --name and --selection
// enforces their mutual exclusion, so its catalogue entry must advertise it. This
// closes the gap where a command binds the shared selection flags (via
// addSelectionFlags) but silently omits the machine-readable conflict.
func TestNameSelectionExclusionIsAdvertisedWhereverBothFlagsExist(t *testing.T) {
	caps := buildCapabilities(newRootCmd())
	sawOne := false
	for _, c := range caps.Commands {
		var hasName, hasSelection bool
		for _, f := range c.Flags {
			switch f.Name {
			case "--name":
				hasName = true
			case "--selection":
				hasSelection = true
			}
		}
		if !(hasName && hasSelection) {
			continue
		}
		sawOne = true
		var advertised bool
		for _, n := range c.Notes {
			if strings.Contains(n, "--name") && strings.Contains(n, "--selection") && strings.Contains(n, "mutually exclusive") {
				advertised = true
			}
		}
		assert.Truef(t, advertised, "command %q exposes both --name and --selection but does not advertise their mutual exclusion", c.Name)
	}
	assert.True(t, sawOne, "expected at least one command to expose both selection flags; test is vacuous otherwise")
}

// containsAll reports whether s contains every substring in subs.
func containsAll(s string, subs []string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// TestValidatorBackedNotesStayInSync ties each curated note that describes a
// CODE-ENFORCED rule back to the guard that enforces it: the guard must reject the
// conflicting/invalid input (so the advertised rule is real), AND some note on the
// command must advertise it (so a rule cobra can't express can't silently drop out
// of guide --json — the core #30 guarantee). Purely descriptive notes (stdout/
// stderr routing, exit codes, --branch-presence semantics) are deliberately NOT
// covered here: they are behaviour locked by their own command tests, and asserting
// a prose substring against itself would add maintenance noise without protecting
// behaviour.
func TestValidatorBackedNotesStayInSync(t *testing.T) {
	cases := []struct {
		name    string
		command string
		guard   func() error // invokes the REAL guard with conflicting/invalid input
		must    []string     // substrings the advertising note must contain
	}{
		{
			name:    "select: --all-repos xor --topic",
			command: "select",
			guard:   func() error { return validateTargeting(targeting{org: "o", allRepos: true, topics: []string{"x"}}) },
			must:    []string{"--all-repos", "--topic", "mutually exclusive"},
		},
		{
			// The other half of the same rule: neither provided is also an error, and
			// the note promises "exactly one ... is required".
			name:    "select: one of --all-repos/--topic required",
			command: "select",
			guard:   func() error { return validateTargeting(targeting{org: "o"}) },
			must:    []string{"--all-repos", "--topic", "required"},
		},
		{
			name:    "mirror: --branch vs --clone-depth",
			command: "mirror",
			guard:   func() error { return validateMirror(mirrorValidation{branch: "dev", cloneDepth: 1}) },
			must:    []string{"--branch", "--clone-depth"},
		},
		{
			name:    "mirror: --workspace xor --purpose",
			command: "mirror",
			guard:   func() error { _, err := resolveWorkspace("/ws", "audit", ""); return err },
			must:    []string{"--workspace", "--purpose", "mutually exclusive"},
		},
		{
			name:    "apply: script required after --",
			command: "apply",
			guard: func() error {
				return validateApply(applyValidation{branch: "b", commitMessage: "m", prTitle: "t", sign: models.SignNone, script: nil})
			},
			must: []string{"script", "--"},
		},
		{
			name:    "apply: --pr-body xor --pr-body-file",
			command: "apply",
			guard:   func() error { _, err := resolvePRBody("inline", "/some/file"); return err },
			must:    []string{"--pr-body", "--pr-body-file", "mutually exclusive"},
		},
		{
			name:    "select: --name rejects traversal",
			command: "select",
			guard:   func() error { _, err := resolveSelectionPath("../evil", ""); return err },
			must:    []string{"--name", ".."},
		},
		{
			// validSelectionName also rejects a bare ".", which the note calls out.
			name:    "select: --name rejects dot",
			command: "select",
			guard:   func() error { _, err := resolveSelectionPath(".", ""); return err },
			must:    []string{"--name", "'.'"},
		},
		{
			name:    "mirror: --purpose safe shape",
			command: "mirror",
			guard:   func() error { return validatePurpose("../evil") },
			must:    []string{"--purpose", ".."},
		},
		{
			// The apply safety guard (real run needs --dry-run=false AND --confirm) is
			// inline in RunE, so exercise it through the command: this fails at the
			// confirm guard before any token or selection is touched.
			name:    "apply: real run needs --confirm",
			command: "apply",
			guard: func() error {
				_, err := executeCmd(t, "", "apply", "--branch", "b", "--commit-message", "m",
					"--pr-title", "t", "--sign", "none", "--dry-run=false", "--", "true")
				return err
			},
			must: []string{"--dry-run=false", "--confirm"},
		},
	}

	caps := buildCapabilities(newRootCmd())
	notesByCmd := map[string][]string{}
	for _, c := range caps.Commands {
		notesByCmd[c.Name] = c.Notes
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Error(t, tc.guard(), "the guard must reject the conflicting/invalid input, or the note describes a rule the code doesn't enforce")
			var advertised bool
			for _, n := range notesByCmd[tc.command] {
				if containsAll(n, tc.must) {
					advertised = true
					break
				}
			}
			assert.Truef(t, advertised, "command %q must advertise a note containing all of %v", tc.command, tc.must)
		})
	}
}

// TestNameShapeIsAdvertisedWhereverNameFlagExists ties the validSelectionName
// constraint to the catalogue: any command exposing --name enforces the safe-name
// shape via resolveSelectionPath, so its entry must advertise it. Structural, like
// the mutual-exclusion guard: a future command that binds --name can't ship without
// stating the rule the machine contract promises.
func TestNameShapeIsAdvertisedWhereverNameFlagExists(t *testing.T) {
	caps := buildCapabilities(newRootCmd())
	sawOne := false
	for _, c := range caps.Commands {
		var hasName bool
		for _, f := range c.Flags {
			if f.Name == "--name" {
				hasName = true
			}
		}
		if !hasName {
			continue
		}
		sawOne = true
		var advertised bool
		for _, n := range c.Notes {
			if strings.Contains(n, "--name") && strings.Contains(n, "..") {
				advertised = true
			}
		}
		assert.Truef(t, advertised, "command %q exposes --name but does not advertise its safe-name shape", c.Name)
	}
	assert.True(t, sawOne, "expected at least one command to expose --name; test is vacuous otherwise")
}

// TestNoCommandHasSubcommands guards the flat-tree assumption buildCapabilities
// relies on: it walks only top-level root.Commands(), so a command that grew
// visible subcommands would be silently under-described — and the completeness
// tests, which share that shallow traversal, would not catch it. The day a
// subcommand appears (e.g. a `workspaces list`/`prune`), this fails loudly and
// forces buildCapabilities to recurse and choose a hierarchical representation,
// rather than shipping a catalogue that omits it.
func TestNoCommandHasSubcommands(t *testing.T) {
	root := newRootCmd()
	for _, cmd := range root.Commands() {
		if isHiddenOrBuiltin(cmd) {
			continue
		}
		for _, sub := range cmd.Commands() {
			if isHiddenOrBuiltin(sub) {
				continue
			}
			t.Fatalf("command %q has subcommand %q: guide --json only catalogues top-level commands — extend buildCapabilities to recurse", cmd.Name(), sub.Name())
		}
	}
}

// TestCuratedExamplesReferenceRealFlags validates the flags in each curated
// example against cobra: an example is part of the machine catalogue, so a flag
// rename that left an example stale (e.g. `mirror --purpose` → `--for`) would ship
// a broken invocation while the other tests passed. Each example must invoke its
// own command, and every --flag token before the bare `--` script separator must
// exist on that command.
func TestCuratedExamplesReferenceRealFlags(t *testing.T) {
	root := newRootCmd()
	for name, cur := range curatedCapabilities {
		require.NotEmptyf(t, cur.example, "command %q needs a curated example", name)
		assert.Truef(t, strings.HasPrefix(cur.example, "goldfinger "+name),
			"example for %q must invoke `goldfinger %s`, got %q", name, name, cur.example)

		cmd, _, err := root.Find([]string{name})
		require.NoError(t, err)
		for _, tok := range strings.Fields(cur.example) {
			if tok == "--" {
				break // everything after -- is the apply script, not goldfinger's flags
			}
			if !strings.HasPrefix(tok, "--") {
				continue
			}
			flag := strings.SplitN(strings.TrimPrefix(tok, "--"), "=", 2)[0]
			assert.NotNilf(t, cmd.Flags().Lookup(flag),
				"example for %q references --%s, which is not a flag on the command", name, flag)
		}
	}
}
