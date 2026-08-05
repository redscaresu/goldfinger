package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// isolateGitEnv points config resolution at a single temp global file and clears
// every env var that would otherwise let the host's real git config, identity
// vars, or injected config leak into a test.
func isolateGitEnv(t *testing.T, globalPath string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", globalPath)
	for _, v := range []string{
		"GIT_CONFIG_SYSTEM", "GIT_CONFIG_COUNT",
		"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL",
		"GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL",
	} {
		t.Setenv(v, "")
		os.Unsetenv(v)
	}
}

func writeGlobalConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gitconfig")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func TestLoadGitConfigParsesIdentityAndSigning(t *testing.T) {
	path := writeGlobalConfig(t, `# a comment
[user]
	name = Ada Lovelace
	email = ada@example.com
	signingkey = ABCD1234
[commit]
	gpgsign = true
[url "https://github.com/"]
	insteadOf = git@github.com:
`)
	isolateGitEnv(t, path)

	cfg := loadGitConfig()
	assert.False(t, cfg.unresolved, "clean config should resolve")

	name, ok := cfg.get("user.name")
	assert.True(t, ok)
	assert.Equal(t, "Ada Lovelace", name)

	email, _ := cfg.get("user.email")
	assert.Equal(t, "ada@example.com", email)

	sign, _ := cfg.get("commit.gpgsign")
	assert.Equal(t, "true", sign)

	key, _ := cfg.get("user.signingkey")
	assert.Equal(t, "ABCD1234", key)
}

func TestLoadGitConfigQuotedValueAndInlineComment(t *testing.T) {
	path := writeGlobalConfig(t, `[user]
	name = "Grace Hopper"  ; quoted, keep spaces
	email = grace@example.com # trailing comment stripped
`)
	isolateGitEnv(t, path)

	cfg := loadGitConfig()
	name, _ := cfg.get("user.name")
	assert.Equal(t, "Grace Hopper", name)
	email, _ := cfg.get("user.email")
	assert.Equal(t, "grace@example.com", email)
}

func TestLoadGitConfigIncludeMarksUnresolved(t *testing.T) {
	path := writeGlobalConfig(t, `[user]
	name = Someone
[include]
	path = ~/.gitconfig-work
`)
	isolateGitEnv(t, path)

	cfg := loadGitConfig()
	assert.True(t, cfg.unresolved, "an include directive must degrade resolution")
	assert.False(t, cfg.parseError, "an unevaluated include is benign uncertainty, not a parse error")
	assert.Contains(t, cfg.reason, "include")
}

func TestLoadGitConfigMalformedLineIsParseError(t *testing.T) {
	// A line with an empty key ("= value") is syntactically broken — git would
	// fatal on it. That is a hard parse error, distinct from a benign include, so
	// doctor must not report a clean pass over it even though user.name parsed.
	path := writeGlobalConfig(t, `[user]
	name = Ada
	= broken
`)
	isolateGitEnv(t, path)

	cfg := loadGitConfig()
	name, _ := cfg.get("user.name")
	assert.Equal(t, "Ada", name, "the valid line before the broken one is still parsed")
	assert.True(t, cfg.unresolved)
	assert.True(t, cfg.parseError, "a malformed line must be a hard parse error")
	assert.Contains(t, cfg.reason, "malformed")
}

func TestLoadGitConfigIncludeIfMarksUnresolved(t *testing.T) {
	path := writeGlobalConfig(t, `[includeIf "gitdir:~/work/"]
	path = ~/.gitconfig-work
`)
	isolateGitEnv(t, path)

	cfg := loadGitConfig()
	assert.True(t, cfg.unresolved)
}

func TestLoadGitConfigEnvIdentityOverrides(t *testing.T) {
	path := writeGlobalConfig(t, `[user]
	name = Config Name
	email = config@example.com
`)
	isolateGitEnv(t, path)
	t.Setenv("GIT_AUTHOR_NAME", "Env Author")

	// With a full pair resolvable (author name from env, everything else from
	// config), the effective identity is the author's: name overridden, email
	// falling back to config user.email — matching git's own resolution.
	cfg := loadGitConfig()
	name, _ := cfg.get("user.name")
	assert.Equal(t, "Env Author", name, "GIT_AUTHOR_NAME overrides config user.name")
	email, _ := cfg.get("user.email")
	assert.Equal(t, "config@example.com", email, "author email falls back to config user.email")
}

func TestLoadGitConfigPartialEnvIdentityDoesNotSynthesise(t *testing.T) {
	// No config identity, and only a partial env pair (author name + committer
	// email). git could NOT commit with this, so doctor must not report an
	// identity: user.name/user.email stay unset.
	isolateGitEnv(t, writeGlobalConfig(t, ""))
	t.Setenv("GIT_AUTHOR_NAME", "Only Author Name")
	t.Setenv("GIT_COMMITTER_EMAIL", "only-committer@example.com")

	cfg := loadGitConfig()
	_, hasName := cfg.get("user.name")
	_, hasEmail := cfg.get("user.email")
	assert.False(t, hasName, "a partial env pair must not synthesise user.name")
	assert.False(t, hasEmail, "a partial env pair must not synthesise user.email")
}

func TestLoadGitConfigCompleteEnvIdentity(t *testing.T) {
	// No config at all, but a complete author+committer pair from env: git can
	// commit, so the effective author identity is recorded.
	isolateGitEnv(t, writeGlobalConfig(t, ""))
	t.Setenv("GIT_AUTHOR_NAME", "Ada")
	t.Setenv("GIT_AUTHOR_EMAIL", "ada@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Ada C")
	t.Setenv("GIT_COMMITTER_EMAIL", "ada-c@example.com")

	cfg := loadGitConfig()
	name, _ := cfg.get("user.name")
	email, _ := cfg.get("user.email")
	assert.Equal(t, "Ada", name)
	assert.Equal(t, "ada@example.com", email)
}

func TestLoadGitConfigEnvInjectedConfig(t *testing.T) {
	path := writeGlobalConfig(t, "")
	isolateGitEnv(t, path)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "user.name")
	t.Setenv("GIT_CONFIG_VALUE_0", "Injected")

	cfg := loadGitConfig()
	name, ok := cfg.get("user.name")
	assert.True(t, ok)
	assert.Equal(t, "Injected", name)
	assert.False(t, cfg.unresolved)
}

func TestLoadGitConfigMissingInjectedValueIsParseError(t *testing.T) {
	// git requires GIT_CONFIG_VALUE_<n> for every declared key; a missing value is
	// a git error, not a silent empty string. doctor must see a hard parse error so
	// it can't report a clean config over one git would reject.
	isolateGitEnv(t, writeGlobalConfig(t, ""))
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "user.name")
	// deliberately do NOT set GIT_CONFIG_VALUE_0
	os.Unsetenv("GIT_CONFIG_VALUE_0")

	cfg := loadGitConfig()
	assert.True(t, cfg.unresolved)
	assert.True(t, cfg.parseError, "a missing injected value is a hard parse error")
	assert.Contains(t, cfg.reason, "GIT_CONFIG_VALUE_0")
	_, ok := cfg.get("user.name")
	assert.False(t, ok, "a key with no value must not be recorded as an empty identity")
}

func TestLoadGitConfigTrailingGarbageCountIsParseError(t *testing.T) {
	// "5x" must be rejected outright, not partially parsed as 5 (git rejects it).
	isolateGitEnv(t, writeGlobalConfig(t, ""))
	t.Setenv("GIT_CONFIG_COUNT", "5x")

	cfg := loadGitConfig()
	assert.True(t, cfg.parseError, "a non-integer GIT_CONFIG_COUNT is a hard parse error")
	assert.Contains(t, cfg.reason, "GIT_CONFIG_COUNT")
}

func TestLoadGitConfigMalformedCountUnresolved(t *testing.T) {
	path := writeGlobalConfig(t, "")
	isolateGitEnv(t, path)
	t.Setenv("GIT_CONFIG_COUNT", "not-a-number")

	cfg := loadGitConfig()
	assert.True(t, cfg.unresolved)
	assert.True(t, cfg.parseError, "a malformed GIT_CONFIG_COUNT is a hard parse error")
	assert.Contains(t, cfg.reason, "GIT_CONFIG_COUNT")
}

func TestLoadGitConfigMissingGlobalIsClean(t *testing.T) {
	// A non-existent global file is the common "no config yet" case and must not
	// mark the config unresolved.
	isolateGitEnv(t, filepath.Join(t.TempDir(), "does-not-exist"))

	cfg := loadGitConfig()
	assert.False(t, cfg.unresolved)
	_, ok := cfg.get("user.name")
	assert.False(t, ok)
}

func TestLoadGitConfigNoSystemHonoured(t *testing.T) {
	// With GIT_CONFIG_NOSYSTEM set, /etc/gitconfig is never in the file list.
	isolateGitEnv(t, writeGlobalConfig(t, ""))
	for _, f := range gitConfigFiles() {
		assert.NotEqual(t, "/etc/gitconfig", f)
	}
}
