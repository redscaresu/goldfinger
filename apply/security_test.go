package apply

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// securityTest marks a test (or fuzz target) as one that locks a security
// invariant of goldfinger. It is a no-op at runtime; its only purpose is
// discoverability, so an auditor can list every security invariant test without
// trusting a curated list — grep the call sites (not the two `func securityTest`
// definitions):
//
//	grep -rn 'securityTest(t)' --include='*_test.go'
//
// Run just them:
//
//	go test ./... -run 'ShellQuote|StripsSourcePAT|ScrubsEveryCredentialVar|DuplicatedDecls|DriftTestCopies|OverridesExistingToken|Refuses|RequiresValidSignMode|SignModeArgs|LocalSign|NeutralisingConfig|NeutralisesAmbient|PinsEmptyGhorgConfig|DisarmsGhorgonly|PinsLayout|PinsAuthRoute|Invocation|RunPatternIsAccurate|ScrubsGitGrandchildVars|PrefixDropStopsAtTheFamily'
//
// That alternation is not maintained by hand — TestSecurityTestRunPatternIsAccurate
// parses it back out of this comment and fails if it misses a marked test or
// starts matching an unmarked one. It had drifted twice before that test existed.
//
// The same one-line marker is defined in each package that holds security
// invariants (currently apply and mirror); keep the two definitions identical.
func securityTest(t testing.TB) { t.Helper() }

// securityTestPackages are the packages whose tests the marker is used in. Named
// here rather than discovered, so adding the marker to a third package is a
// deliberate edit that also updates the audit recipe above.
var securityTestPackages = []string{"apply", "mirror"}

// runPatternRE pulls the alternation out of the `go test ... -run '...'` line in
// this file's own doc comment above.
var runPatternRE = regexp.MustCompile(`go test \./\.\.\. -run '([^']+)'`)

// TestSecurityTestRunPatternIsAccurate keeps the audit recipe honest. The recipe
// is a curated list of name fragments, which is precisely the shape that rots:
// rename a test and the auditor who trusts the comment silently runs fewer checks
// than they think, with no failure anywhere. It had already lost five tests to two
// renames before this test existed.
//
// Both halves matter. Matching every marked test is the obvious one; NOT matching
// the unmarked ones is what stops the rot being "fixed" with something like
// -run 'Test' — which would pass the first half while making the recipe useless.
func TestSecurityTestRunPatternIsAccurate(t *testing.T) {
	securityTest(t)

	self, err := os.ReadFile("security_test.go")
	require.NoError(t, err)
	m := runPatternRE.FindSubmatch(self)
	require.NotNil(t, m, "the doc comment on securityTest must carry a `go test ./... -run '...'` recipe")
	re, err := regexp.Compile(string(m[1]))
	require.NoError(t, err, "the recipe's -run pattern must be a valid regexp")

	marked, unmarked := testFuncsByMarker(t, securityTestPackages)
	require.NotEmpty(t, marked)
	require.NotEmpty(t, unmarked)

	for _, name := range marked {
		assert.Truef(t, re.MatchString(name),
			"%s calls securityTest but the -run recipe misses it — add a fragment that matches it", name)
	}
	for _, name := range unmarked {
		assert.Falsef(t, re.MatchString(name),
			"the -run recipe matches %s, which is not a security-invariant test — the recipe must stay selective, not widen to catch renames", name)
	}
}

// testFuncsByMarker splits the Test/Fuzz functions in the given sibling packages
// by whether their body calls securityTest, using the AST rather than a text
// scan so a name in a comment or string can't be mistaken for a call.
func testFuncsByMarker(t *testing.T, pkgs []string) (marked, unmarked []string) {
	t.Helper()
	for _, pkg := range pkgs {
		paths, err := filepath.Glob(filepath.Join("..", pkg, "*_test.go"))
		require.NoError(t, err)
		require.NotEmptyf(t, paths, "no test files found for package %s", pkg)
		for _, path := range paths {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			require.NoErrorf(t, err, "parse %s", path)
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv != nil || fn.Body == nil {
					continue
				}
				name := fn.Name.Name
				if !strings.HasPrefix(name, "Test") && !strings.HasPrefix(name, "Fuzz") {
					continue
				}
				if callsSecurityTest(fn.Body) {
					marked = append(marked, name)
				} else {
					unmarked = append(unmarked, name)
				}
			}
		}
	}
	return marked, unmarked
}

// callsSecurityTest reports whether the body contains a call to securityTest,
// at any depth (subtests mark themselves in the outer func, but nesting is
// cheaper to allow than to forbid).
func callsSecurityTest(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "securityTest" {
			found = true
			return false
		}
		return true
	})
	return found
}

// maxFuzzToken caps the token length FuzzShellQuote will execute. The quoting
// logic is length-independent, so a few KiB exercises it fully; the cap keeps a
// very long fuzz string from hitting the OS argv limit (ARG_MAX/E2BIG) and
// failing the exec for a reason unrelated to quoting — a false failure.
const maxFuzzToken = 4096

// FuzzShellQuote proves the central injection defence: no operator-supplied
// token can break out of the quoting apply.writeScript applies before handing
// the command to multi-gitter. Rather than assert a property of the quoted
// *string*, it exercises the real assembly end to end — writeScript builds the
// actual `#!/bin/sh` script, a real POSIX sh runs it, and we confirm the fuzzed
// token reaches the program as exactly one argument, byte-for-byte, without
// merging into, displacing, or executing anything around it.
//
// The program under the script is /usr/bin/printf, invoked as
//
//	printf '%s\n' <fuzzed-token> SENTINEL
//
// so the output must be exactly "<token>\nSENTINEL\n". Any quote break-out would
// either corrupt the first line, drop/merge the SENTINEL word, or (the danger
// case) run an injected command — all of which fail the equality assertion.
//
// This is an argv-round-trip oracle under a real shell, not a full proof that no
// shell side effect ran: it observes stdout and exit status, so a *silent*
// command substitution wouldn't be seen directly. That's an accepted limit —
// the current shellQuote can't produce one, and any regression that let a
// substitution escape would also corrupt the round-trip and fail here.
//
// Blast-radius containment: `go test` (including CI) only ever executes the
// curated seed corpus below — all harmless echo-based payloads. Active fuzzing
// (`-fuzz`) generates unconstrained mutations, so IF shellQuote ever regressed a
// mutation could get a command substitution past the quotes. To keep that from
// touching anything real, each script runs with an emptied environment (`PATH=`
// only narrows ordinary PATH lookup — it does NOT sandbox: shell builtins like
// `kill`, `command -p`, and redirections to absolute paths can still reach
// outside), HOME and the working directory pointed at a throwaway temp dir, and a
// hard timeout so a hang can't wedge the run. Those reduce incidental damage but
// are not containment; the real guarantee is procedural — run active fuzzing in a
// disposable environment (throwaway VM/container), never on a real host.
func FuzzShellQuote(f *testing.F) {
	seeds := []string{
		"",
		"plain",
		"it's",
		"a b c",
		"a\tb",
		"a\nb",
		`$(echo pwned)`,
		"`echo pwned`",
		"; echo pwned",
		"&& echo pwned",
		"| echo pwned",
		"' ; echo pwned ; '",
		`'\''`,
		`\`,
		`"`,
		"*",
		"?",
		"~",
		"${HOME}",
		"--flag=value",
		"-rf",
		"newline\nand more",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, s string) {
		securityTest(t)
		// A NUL byte can't survive a POSIX argv (execve truncates at it), so it
		// isn't a realistic apply token; skip rather than assert on it.
		if strings.ContainsRune(s, 0) {
			t.Skip("NUL cannot appear in a POSIX argument list")
		}
		if len(s) > maxFuzzToken {
			t.Skip("token longer than the argv limit is out of scope for quote parsing")
		}

		const sentinel = "SENTINEL"
		path, cleanup, err := writeScript([]string{"/usr/bin/printf", "%s\n", s, sentinel})
		require.NoError(t, err)
		defer cleanup()

		// Reduce incidental blast radius if a future shellQuote regression let a
		// fuzzed input escape: PATH= (narrows ordinary command lookup only — not a
		// sandbox; builtins/`command -p`/absolute-path redirections still reach
		// out), HOME and CWD in a throwaway dir, and a hard timeout against hangs.
		// Real containment is procedural: run -fuzz in a disposable environment.
		sandbox := t.TempDir()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/bin/sh", path)
		cmd.Dir = sandbox
		cmd.Env = []string{"PATH=", "HOME=" + sandbox}

		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "script failed for %q: %s", s, out)
		assert.Equalf(t, s+"\n"+sentinel+"\n", string(out),
			"token %q did not round-trip as a single argument — possible quote break-out", s)
	})
}

// assertGitEnvScrubbed asserts a hostile git environment reached none of a
// delegate's child environment, and that the scrub did not overreach past the four
// families it covers. It exists in both packages because the exposure does: ghorg
// always execs git, and multi-gitter execs it under --git-type=cmd, which is what
// --sign=local selects — and neither ever sets cmd.Env, so git inherits whatever
// goldfinger built, with the PAT already in the clone URL it is being handed.
//
// Every name here is a STRING LITERAL, and deliberately NOT read from
// models.GitGrandchildEnvVars: that list holds PREFIXES, so a test that ranged
// over it would set variables literally called "GIT_TRACE*" and
// "GIT_CONFIG_KEY_*" — which no git build reads — and then assert that the things
// it had just invented were removed. The names below are real members a real git
// honours, chosen to span all four families: the tracing knobs that write the
// token-bearing argv to a path the host picked (including the switch that DISABLES
// redaction, the one that dumps other variables' values, and the pre-prefix
// spelling that only an exact-name entry catches), the config-injection channels
// that rewrite the remote, the transport knob that stops git checking who it is
// talking to, the six that point git at a repository other than the clone the
// delegate chose by setting cmd.Dir, and the three that make git serve content or
// ancestry the selected repository does not have.
//
// The survivors matter as much as the casualties. A prefix is the one scrub that
// can silently grow, so this pins its edges: GITHUB_ACTIONS proves the prefix is
// GIT_TRACE and not GIT; GIT_CONFIG_GLOBAL proves the config scrub is the injection
// channels and not GIT_CONFIG generally — that variable is where an operator's
// commit.gpgsign may live, so scrubbing it would silently unsign a run that
// announced --sign=local; GIT_AUTHOR_NAME is the identity git may have no other
// source for, so scrubbing it fails the commit outright in a CI-shaped environment;
// and GIT_TERMINAL_PROMPT=0 is what turns a bad credential into a fast failure
// instead of a delegate blocked on a prompt nobody can see. All four exclusions are
// argued in full on models.gitGrandchildEnvVars; asserting them here is what stops a
// later "while we're at it" widening from landing quietly.
func assertGitEnvScrubbed(t *testing.T, env []string) {
	t.Helper()
	for _, name := range []string{
		"GIT_TRACE",
		"GIT_TRACE2",
		"GIT_TRACE2_EVENT",
		"GIT_TRACE_CURL",
		"GIT_TRACE_REDACT",
		"GIT_TRACE2_ENV_VARS",
		"GIT_CURL_VERBOSE",
		"GIT_CONFIG_COUNT",
		"GIT_CONFIG_KEY_0",
		"GIT_CONFIG_VALUE_0",
		"GIT_CONFIG_PARAMETERS",
		"GIT_SSL_NO_VERIFY",
		"GIT_DIR",
		"GIT_WORK_TREE",
		"GIT_INDEX_FILE",
		"GIT_OBJECT_DIRECTORY",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES",
		"GIT_NAMESPACE",
		"GIT_REPLACE_REF_BASE",
		"GIT_SHALLOW_FILE",
		"GIT_GRAFT_FILE",
	} {
		for _, e := range env {
			assert.NotContainsf(t, e, name+"=",
				"%s must be stripped: git is a grandchild here, handed the PAT inside the clone URL, and this variable makes it disclose that URL, send it to a host goldfinger never resolved, act on a repository other than the clone, or serve content the selected repository does not have", name)
		}
	}
	assert.Contains(t, env, "GITHUB_ACTIONS=keep-me",
		"the prefix is GIT_TRACE, not GIT — a scrub that eats GITHUB_* has overreached")
	assert.Contains(t, env, "GIT_CONFIG_GLOBAL=keep-me",
		"only the GIT_CONFIG_COUNT/KEY_n/VALUE_n/PARAMETERS injection channels are in scope; GIT_CONFIG_GLOBAL is where --sign=local's commit.gpgsign may live, and scrubbing it would silently unsign a run that announced signing")
	assert.Contains(t, env, "GIT_AUTHOR_NAME=keep-me",
		"the identity vars are announced rather than removed (doctor folds them into the reported identity); scrubbing them leaves git with no identity at all where the environment is the only source, and the commit fails")
	assert.Contains(t, env, "GIT_TERMINAL_PROMPT=0",
		"hardening switches are never scrubbed — removing GIT_TERMINAL_PROMPT=0 would let a bad credential hang a delegate on a prompt nobody can see")
}

// setGitHostileEnv exports the same names assertGitEnvScrubbed checks for, plus the
// survivors, so those assertions run against an environment that genuinely held
// every one of them — an assertion against a variable that was never set passes for
// free. Each hostile value is the shape the real attack takes, not a placeholder, so
// the test reads as the scenario it defends against. Kept beside its assertion, and
// identical in both packages, for the same reason the assertion is.
func setGitHostileEnv(t *testing.T) {
	t.Helper()
	for name, val := range map[string]string{
		"GIT_TRACE":                        "/tmp/goldfinger-hostile-trace.log",
		"GIT_TRACE2":                       "/tmp/goldfinger-hostile-trace.log",
		"GIT_TRACE2_EVENT":                 "/tmp/goldfinger-hostile-trace.log",
		"GIT_TRACE_CURL":                   "1",
		"GIT_TRACE_REDACT":                 "0",
		"GIT_TRACE2_ENV_VARS":              "GHORG_GITHUB_TOKEN,GITHUB_TOKEN",
		"GIT_CURL_VERBOSE":                 "1",
		"GIT_CONFIG_COUNT":                 "1",
		"GIT_CONFIG_KEY_0":                 "url.https://attacker.example/.insteadOf",
		"GIT_CONFIG_VALUE_0":               "https://oauth2:",
		"GIT_CONFIG_PARAMETERS":            "'url.https://attacker.example/.insteadOf=https://oauth2:'",
		"GIT_SSL_NO_VERIFY":                "false",
		"GIT_DIR":                          "/tmp/goldfinger-hostile-repo/.git",
		"GIT_WORK_TREE":                    "/tmp/goldfinger-hostile-worktree",
		"GIT_INDEX_FILE":                   "/tmp/goldfinger-hostile-index",
		"GIT_OBJECT_DIRECTORY":             "/tmp/goldfinger-hostile-objects",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES": "/tmp/goldfinger-donor/.git/objects",
		"GIT_NAMESPACE":                    "hostile",
		"GIT_REPLACE_REF_BASE":             "refs/hostile-replace",
		"GIT_SHALLOW_FILE":                 "/tmp/goldfinger-hostile-shallow",
		"GIT_GRAFT_FILE":                   "/tmp/goldfinger-hostile-grafts",
		"GITHUB_ACTIONS":                   "keep-me",
		"GIT_CONFIG_GLOBAL":                "keep-me",
		"GIT_AUTHOR_NAME":                  "keep-me",
		"GIT_TERMINAL_PROMPT":              "0",
	} {
		t.Setenv(name, val)
	}
}
