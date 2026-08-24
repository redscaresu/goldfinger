package models

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRepoFullName(t *testing.T) {
	r := Repo{Owner: "redscaresu", Name: "goldfinger"}
	assert.Equal(t, "redscaresu/goldfinger", r.FullName())
}

// TestCredentialEnvVarsCoversEveryTokenVar pins the membership the scrub depends
// on: each delegate strips the whole set and then sets back its own token var, so
// a variable missing here would be handed to a child unscrubbed. ghorg's GitHub
// App variables are here because they are an alternative identity: an ambient set
// would let ghorg act as an App while goldfinger announces a PAT principal.
// GHORG_TOKEN_CMD is here because it is a credential *source* — ghorg runs it as
// `sh -c` and uses the output as the token.
//
// Every name is a STRING LITERAL, including the four that have a constant. Naming
// the constants would make the test derive its expectation from the thing it is
// meant to pin: rename MultiGitterTokenEnvVar's value and both sides move
// together, so the test still passes while the scrub stops stripping the variable
// that actually matters. These four values are an external contract, not
// goldfinger's choice — multi-gitter reads GITHUB_TOKEN (cmd/other.go:38) and
// ghorg reads GHORG_GITHUB_TOKEN (configs/configs.go:330, :457), so a drift is
// silent in both directions at once: the delegate gets no token under the name it
// reads, AND an ambient one under the real name survives the scrub.
func TestCredentialEnvVarsCoversEveryTokenVar(t *testing.T) {
	assert.ElementsMatch(t, []string{
		"GOLD_FINGER_PAT",
		"GITHUB_TOKEN",
		"GHORG_GITHUB_TOKEN",
		"GH_TOKEN",
		"GHORG_GITHUB_APP_ID",
		"GHORG_GITHUB_APP_INSTALLATION_ID",
		"GHORG_GITHUB_APP_PEM_PATH",
		"GHORG_GITHUB_TOKEN_FROM_GITHUB_APP",
		"GHORG_TOKEN_CMD",
	}, CredentialEnvVars())
}

// TestTokenEnvVarConstantsMatchTheDelegateContract states the four values a second
// time, as the constants rather than as list membership, so the failure message
// says which name drifted instead of only that a set no longer matches. GH_TOKEN
// and GOLD_FINGER_PAT are goldfinger's own inputs; the other two are the names the
// delegates read and cannot be chosen freely.
func TestTokenEnvVarConstantsMatchTheDelegateContract(t *testing.T) {
	assert.Equal(t, "GOLD_FINGER_PAT", TokenEnvVar)
	assert.Equal(t, "GITHUB_TOKEN", MultiGitterTokenEnvVar, "multi-gitter reads this name and no other (cmd/other.go:38)")
	assert.Equal(t, "GHORG_GITHUB_TOKEN", GhorgTokenEnvVar, "ghorg reads this name and no other (configs/configs.go:330)")
	assert.Equal(t, "GH_TOKEN", GHTokenEnvVar)
}

// TestGitGrandchildEnvVarsPinsEveryFamily states the set as literals for the same
// reason the credential set is stated as literals: a test that read the package
// variable would pass no matter what it held.
//
// The "*" is load-bearing and is asserted here rather than left implicit in
// overrideEnv. Dropping it from "GIT_TRACE*" narrows the entry to the single
// variable GIT_TRACE while every GIT_TRACE2* knob keeps leaking; dropping it from
// "GIT_CONFIG_KEY_*" is worse, because GIT_CONFIG_KEY_0 is the name git actually
// reads, so an exact-match entry would scrub nothing at all.
//
// The ABSENCE of a "*" is load-bearing in the other direction and is why the
// repo-target names are spelled exactly. "GIT_DIR*" would reach nothing extra that
// git reads, and "GIT_CONFIG*" would swallow GIT_CONFIG_GLOBAL — a variable this
// list deliberately preserves, because scrubbing it re-enables ~/.gitconfig and
// unsigns a --sign=local run. The same holds for the identity and hardening names:
// anything broader than the entries below would take GIT_AUTHOR_NAME (which git
// needs to commit at all in a CI-shaped environment) or GIT_TERMINAL_PROMPT=0 (which
// is what stops a bad credential hanging a delegate). A widening is the failure mode
// a prefix scrub invites, so both edges are pinned: here by name, and in each
// package's security test by an assertion that the excluded variables survive.
func TestGitGrandchildEnvVarsPinsEveryFamily(t *testing.T) {
	assert.ElementsMatch(t, []string{
		"GIT_TRACE*",
		"GIT_CURL_VERBOSE",
		"GIT_CONFIG_COUNT",
		"GIT_CONFIG_KEY_*",
		"GIT_CONFIG_VALUE_*",
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
	}, GitGrandchildEnvVars())
}

func TestGitGrandchildEnvVarsReturnsCopy(t *testing.T) {
	got := GitGrandchildEnvVars()
	got[0] = "MUTATED"
	assert.NotContains(t, GitGrandchildEnvVars(), "MUTATED")
}

// TestCredentialEnvVarsReturnsCopy proves a caller cannot shrink the canonical
// list — the same protection validSignModes has, for the same reason: apply and
// mirror trust this set to be complete.
func TestCredentialEnvVarsReturnsCopy(t *testing.T) {
	got := CredentialEnvVars()
	got[0] = "MUTATED"
	assert.NotContains(t, CredentialEnvVars(), "MUTATED")
}
