package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// gitConfig is a best-effort, git-binary-free view of the git configuration that
// will actually reach multi-gitter's temporary checkouts: system + global + env,
// but NOT the caller's local .git/config (multi-gitter clones each repo into its
// own checkout, so a local identity does not propagate). It exists because the
// charter forbids goldfinger from running `git` itself (AGENTS.md) — doctor must
// therefore parse config files directly.
//
// values is keyed by a normalised "section.key" (lowercased section/subsection and
// key, per git's case-insensitive section/name matching); the last write wins,
// matching git's precedence when files are read low→high priority.
//
// unresolved is set when the config contains a construct this parser does not
// faithfully evaluate. It has two flavours, which doctor must treat differently:
//
//   - a benign `include`/`includeIf` directive: the values we did read are almost
//     certainly what git uses, but an include *could* override them — advisory
//     uncertainty (unresolved=true, parseError=false).
//   - a hard problem — a malformed section/line, an unreadable file, or malformed
//     env-injected config (GIT_CONFIG_COUNT): git itself may reject this config, so
//     a "pass" here would be a false pass (unresolved=true, parseError=true).
//
// reason carries the human detail for whichever was last recorded (a hard problem
// takes precedence over an include for messaging).
type gitConfig struct {
	values     map[string]string
	unresolved bool
	parseError bool
	reason     string
}

// markInclude records that an unevaluated include/includeIf was seen — advisory
// uncertainty only. It never downgrades an already-recorded hard parseError.
func (c *gitConfig) markInclude(reason string) {
	c.unresolved = true
	if !c.parseError {
		c.reason = reason
	}
}

// markParseError records a hard problem that git itself might reject. It always
// wins the reason, since it is strictly more serious than an include.
func (c *gitConfig) markParseError(reason string) {
	c.unresolved = true
	c.parseError = true
	c.reason = reason
}

// get returns the value for a normalised "section.key" and whether it was set.
func (c gitConfig) get(key string) (string, bool) {
	v, ok := c.values[strings.ToLower(key)]
	return v, ok
}

// gitBool reports whether a git-config value is truthy. git accepts true/yes/on/1
// (case-insensitive), and a bare key with no value is also true (our parser stores
// that as "true"). Anything else — including an unset key — is false.
func gitBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "yes", "on", "1":
		return true
	default:
		return false
	}
}

// loadGitConfig reads git configuration the way git itself would for a commit in a
// fresh checkout, in increasing-precedence order: system, then global, then
// env-injected (GIT_CONFIG_COUNT). It never reads a repo-local .git/config and
// never runs git. Identity env vars (GIT_AUTHOR_*/GIT_COMMITTER_*) are folded in
// as the highest-precedence source for user.name/user.email, since git honours
// them for authoring.
func loadGitConfig() gitConfig {
	cfg := gitConfig{values: map[string]string{}}

	for _, path := range gitConfigFiles() {
		if path == "" {
			continue
		}
		if err := parseGitConfigFile(path, &cfg); err != nil {
			// A missing file is normal (no global config yet); only a genuine read
			// or parse problem degrades resolution.
			if !os.IsNotExist(err) {
				cfg.markParseError(fmt.Sprintf("could not read %s: %v", path, err))
			}
		}
	}

	// Env-injected config (GIT_CONFIG_COUNT / GIT_CONFIG_KEY_n / GIT_CONFIG_VALUE_n)
	// has the highest file-like precedence in git; apply it last.
	applyEnvInjectedConfig(&cfg)

	// Fold in GIT_AUTHOR_*/GIT_COMMITTER_*. git needs a *complete* pair — author
	// name+email AND committer name+email — to make a commit, each falling back to
	// user.name/user.email. We record an effective user.name/user.email only when
	// all four resolve, so a partial set (e.g. GIT_AUTHOR_NAME + GIT_COMMITTER_EMAIL
	// with nothing else) never synthesises an identity git wouldn't actually have.
	applyEnvIdentity(&cfg)

	return cfg
}

// applyEnvIdentity resolves the effective committing identity from config plus the
// GIT_AUTHOR_*/GIT_COMMITTER_* env vars and, only if a full author+committer pair
// resolves, writes it back as user.name/user.email. A partial override leaves the
// (possibly incomplete) config values untouched, so the identity check sees the
// truth: git could not commit.
func applyEnvIdentity(cfg *gitConfig) {
	cfgName := strings.TrimSpace(cfg.values["user.name"])
	cfgEmail := strings.TrimSpace(cfg.values["user.email"])
	authorName := envOr("GIT_AUTHOR_NAME", cfgName)
	authorEmail := envOr("GIT_AUTHOR_EMAIL", cfgEmail)
	committerName := envOr("GIT_COMMITTER_NAME", cfgName)
	committerEmail := envOr("GIT_COMMITTER_EMAIL", cfgEmail)
	if authorName == "" || authorEmail == "" || committerName == "" || committerEmail == "" {
		return
	}
	// The author identity is what appears on the commit; record it as the
	// effective user.name/user.email.
	cfg.values["user.name"] = authorName
	cfg.values["user.email"] = authorEmail
}

// envOr returns the trimmed value of env var name, or fallback when it is unset
// or blank.
func envOr(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

// gitConfigFiles returns the system and global config paths in increasing
// precedence, honouring the env overrides git respects: GIT_CONFIG_NOSYSTEM
// suppresses system config, GIT_CONFIG_SYSTEM/GIT_CONFIG_GLOBAL relocate them, and
// XDG_CONFIG_HOME provides the fallback global path.
func gitConfigFiles() []string {
	var files []string

	// System config, unless suppressed.
	if os.Getenv("GIT_CONFIG_NOSYSTEM") == "" {
		if sys := os.Getenv("GIT_CONFIG_SYSTEM"); sys != "" {
			files = append(files, sys)
		} else {
			files = append(files, "/etc/gitconfig")
		}
	}

	// Global config: an explicit GIT_CONFIG_GLOBAL wins; otherwise ~/.gitconfig and
	// the XDG path (git reads both, ~/.gitconfig taking precedence, so append XDG
	// first then ~/.gitconfig).
	if glb := os.Getenv("GIT_CONFIG_GLOBAL"); glb != "" {
		files = append(files, glb)
	} else {
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			files = append(files, filepath.Join(xdg, "git", "config"))
		} else if home, err := os.UserHomeDir(); err == nil {
			files = append(files, filepath.Join(home, ".config", "git", "config"))
		}
		if home, err := os.UserHomeDir(); err == nil {
			files = append(files, filepath.Join(home, ".gitconfig"))
		}
	}
	return files
}

// applyEnvInjectedConfig folds GIT_CONFIG_COUNT-style env config into cfg. Each
// key is of the form "section.key" or "section.subsection.key". git requires every
// GIT_CONFIG_KEY_<n> AND GIT_CONFIG_VALUE_<n> in 0..count-1 to be present and the
// count to be a clean integer; any deviation is a git error, so we mark a hard
// parseError rather than guessing (a silently-absent value would otherwise become
// "", a false clean read).
func applyEnvInjectedConfig(cfg *gitConfig) {
	countStr := os.Getenv("GIT_CONFIG_COUNT")
	if countStr == "" {
		return
	}
	// strconv.Atoi (not fmt.Sscanf) so trailing garbage like "5x" is rejected
	// rather than partially parsed as 5.
	count, err := strconv.Atoi(strings.TrimSpace(countStr))
	if err != nil || count < 0 {
		cfg.markParseError("malformed GIT_CONFIG_COUNT")
		return
	}
	for i := 0; i < count; i++ {
		key, keyOK := os.LookupEnv(fmt.Sprintf("GIT_CONFIG_KEY_%d", i))
		val, valOK := os.LookupEnv(fmt.Sprintf("GIT_CONFIG_VALUE_%d", i))
		// git treats a missing key OR a missing value as an error — both must be
		// set. os.LookupEnv distinguishes unset from a legitimately empty value.
		if !keyOK || key == "" {
			cfg.markParseError(fmt.Sprintf("missing GIT_CONFIG_KEY_%d", i))
			continue
		}
		if !valOK {
			cfg.markParseError(fmt.Sprintf("missing GIT_CONFIG_VALUE_%d", i))
			continue
		}
		cfg.values[strings.ToLower(key)] = val
	}
}

// parseGitConfigFile parses a single git-config (INI-like) file into cfg.values.
// It handles [section], [section "subsection"], key = value, comments (# / ;), and
// quoted values. It intentionally does NOT evaluate include/includeIf — if one is
// seen, resolution is marked unresolved so doctor can warn rather than mislead.
func parseGitConfigFile(path string, cfg *gitConfig) error {
	f, err := os.Open(path) //nolint:gosec // G304: path is a git-config location goldfinger resolves itself (GIT_CONFIG paths / ~/.gitconfig) for read-only doctor inspection, not attacker-controlled.
	if err != nil {
		return err
	}
	defer f.Close()

	var section string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			end := strings.Index(line, "]")
			if end < 0 {
				cfg.markParseError("malformed section header in " + path)
				continue
			}
			section = parseSectionHeader(line[1:end])
			// include / includeIf pull in other files we don't evaluate. This is
			// benign uncertainty, not a config error, so it never fails a check that
			// otherwise passes — it only annotates the detail.
			if section == "include" || strings.HasPrefix(section, "includeif") {
				cfg.markInclude("include/includeIf directive not evaluated (in " + path + ")")
			}
			continue
		}
		key, val, ok := parseGitConfigKV(line)
		if !ok || section == "" {
			cfg.markParseError("malformed config line in " + path)
			continue
		}
		cfg.values[section+"."+strings.ToLower(key)] = val
	}
	return sc.Err()
}

// parseSectionHeader normalises a section header body. `[user]` -> "user";
// `[include "path"]` -> `include`; `[url "https://x"]` -> `url.https://x` with the
// subsection kept case-sensitive per git, though we only need the section name for
// the include check and lowercased section for keys.
func parseSectionHeader(body string) string {
	body = strings.TrimSpace(body)
	if i := strings.Index(body, "\""); i >= 0 {
		name := strings.ToLower(strings.TrimSpace(body[:i]))
		sub := strings.Trim(body[i:], "\"")
		sub = strings.Trim(sub, "\"")
		return name + "." + sub
	}
	return strings.ToLower(body)
}

// parseGitConfigKV splits a "key = value" line, trimming surrounding quotes and
// inline comments outside quotes. A bare key (no "=") means boolean true, as git
// treats it.
func parseGitConfigKV(line string) (key, val string, ok bool) {
	eq := strings.Index(line, "=")
	if eq < 0 {
		return strings.TrimSpace(line), "true", true
	}
	key = strings.TrimSpace(line[:eq])
	val = strings.TrimSpace(line[eq+1:])
	if key == "" {
		return "", "", false
	}
	if strings.HasPrefix(val, "\"") {
		// Quoted value: take up to the closing quote.
		if end := strings.Index(val[1:], "\""); end >= 0 {
			val = val[1 : end+1]
			return key, val, true
		}
	}
	// Strip an inline comment on an unquoted value.
	if i := strings.IndexAny(val, "#;"); i >= 0 {
		val = strings.TrimSpace(val[:i])
	}
	return key, val, true
}
