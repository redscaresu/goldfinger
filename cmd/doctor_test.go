package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-github/v89/github"
	"github.com/redscaresu/goldfinger/client"
	"github.com/redscaresu/goldfinger/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// okDeps returns a doctorDeps where every check passes, so individual tests can
// override just the one behaviour they exercise.
func okDeps() doctorDeps {
	return doctorDeps{
		resolveToken: func(context.Context) (string, string, error) {
			return "tok", tokenSourceEnv, nil
		},
		verifyLogin: func(context.Context, string) (string, error) {
			return "octocat", nil
		},
		readQuota: func(context.Context, string) (client.Quota, error) {
			return client.Quota{Limit: 5000, Remaining: 4900, Reset: time.Unix(1700000000, 0)}, nil
		},
		probeTool: func(_ context.Context, name string) (string, string, bool) {
			return "/usr/local/bin/" + name, name + " v1.2.3", true
		},
		loadConfig: func() gitConfig {
			return gitConfig{values: map[string]string{
				"user.name":       "Ada",
				"user.email":      "ada@example.com",
				"commit.gpgsign":  "true",
				"user.signingkey": "KEY",
			}}
		},
	}
}

func runDoctorCapture(t *testing.T, deps doctorDeps, asJSON bool) (out, errOut string, err error) {
	t.Helper()
	return runDoctorCaptureOpts(t, deps, doctorOpts{asJSON: asJSON})
}

func runDoctorCaptureOpts(t *testing.T, deps doctorDeps, opts doctorOpts) (out, errOut string, err error) {
	t.Helper()
	var o, e bytes.Buffer
	err = runDoctor(context.Background(), deps, opts, &o, &e)
	return o.String(), e.String(), err
}

func TestRunDoctorAllOK(t *testing.T) {
	out, _, err := runDoctorCapture(t, okDeps(), false)
	require.NoError(t, err)
	assert.Contains(t, out, "authenticated as octocat (via "+tokenSourceEnv+")")
	assert.Contains(t, out, "[ok] ghorg")
	assert.Contains(t, out, "[ok] multi-gitter")
	assert.Contains(t, out, "Ada <ada@example.com>")
	assert.NotContains(t, out, "[fail]")
}

func TestRunDoctorNoTokenFails(t *testing.T) {
	deps := okDeps()
	deps.resolveToken = func(context.Context) (string, string, error) {
		return "", "", errors.New("no GitHub token found")
	}
	out, _, err := runDoctorCapture(t, deps, false)

	var ee exitError
	require.True(t, errors.As(err, &ee), "a failed check must set a non-zero exit")
	assert.Equal(t, 1, ee.code)
	assert.Contains(t, out, "[fail] auth")
}

func TestRunDoctorVerifyFailure(t *testing.T) {
	deps := okDeps()
	deps.verifyLogin = func(context.Context, string) (string, error) {
		return "", errors.New("401 Bad credentials")
	}
	out, _, err := runDoctorCapture(t, deps, false)

	assert.Equal(t, 1, exitCode(err))
	assert.Contains(t, out, "[fail] auth")
	assert.Contains(t, out, "did not verify")
}

func TestRunDoctorMissingToolFails(t *testing.T) {
	deps := okDeps()
	deps.probeTool = func(_ context.Context, name string) (string, string, bool) {
		if name == "ghorg" {
			return "", "", false
		}
		return "/usr/local/bin/" + name, "", true
	}
	out, _, err := runDoctorCapture(t, deps, false)

	assert.Equal(t, 1, exitCode(err))
	assert.Contains(t, out, "[fail] ghorg: ghorg not found on PATH")
	assert.Contains(t, out, "gabrie30/ghorg")
}

func TestRunDoctorAmbientShadowWarns(t *testing.T) {
	// Shadow warning fires only when the token came from the gh session AND an
	// ambient token is set.
	t.Setenv("GITHUB_TOKEN", "ambient-value")
	deps := okDeps()
	deps.resolveToken = func(context.Context) (string, string, error) {
		return "tok", tokenSourceGh, nil
	}
	out, _, err := runDoctorCapture(t, deps, false)

	require.NoError(t, err, "a warn must not fail the run")
	assert.Contains(t, out, "[warn] auth-shadow")
}

func TestRunDoctorNoShadowWhenPAT(t *testing.T) {
	// Even with an ambient token present, a GOLD_FINGER_PAT source is unaffected.
	t.Setenv("GITHUB_TOKEN", "ambient-value")
	out, _, err := runDoctorCapture(t, okDeps(), false)
	require.NoError(t, err)
	assert.Contains(t, out, "[ok] auth-shadow")
}

func TestRunDoctorGitIdentityWarn(t *testing.T) {
	deps := okDeps()
	deps.loadConfig = func() gitConfig {
		return gitConfig{values: map[string]string{}}
	}
	out, _, err := runDoctorCapture(t, deps, false)

	require.NoError(t, err, "a missing identity is a warn, not a fail")
	assert.Contains(t, out, "[warn] git-identity")
	assert.Contains(t, out, "make no commit")
}

func TestRunDoctorJSONShapeAndNoToken(t *testing.T) {
	deps := okDeps()
	const secret = "super-secret-token-value"
	deps.resolveToken = func(context.Context) (string, string, error) {
		return secret, tokenSourceEnv, nil
	}
	out, errOut, err := runDoctorCapture(t, deps, true)
	require.NoError(t, err)

	assert.NotContains(t, out, secret, "the token value must never appear in output")
	assert.NotContains(t, errOut, secret)

	var rep doctorReport
	require.NoError(t, json.Unmarshal([]byte(out), &rep))
	assert.Equal(t, doctorReportVersion, rep.Version)

	byName := map[string]doctorCheck{}
	for _, c := range rep.Checks {
		byName[c.Check] = c
	}
	assert.Equal(t, statusOK, byName["auth"].Status)
	assert.Equal(t, statusOK, byName["ghorg"].Status)
	assert.Equal(t, statusOK, byName["multi-gitter"].Status)
	assert.Equal(t, statusOK, byName["git-identity"].Status)
	assert.Contains(t, []string{statusOK, statusInfo, statusWarn}, byName["signing"].Status)
}

func TestRunDoctorQuiet(t *testing.T) {
	t.Run("non-json emits nothing but keeps exit code", func(t *testing.T) {
		out, errOut, err := runDoctorCaptureOpts(t, okDeps(), doctorOpts{quiet: true})
		require.NoError(t, err)
		assert.Empty(t, out)
		assert.Empty(t, errOut)
	})

	t.Run("failed check emits nothing and exits 1", func(t *testing.T) {
		deps := okDeps()
		deps.resolveToken = func(context.Context) (string, string, error) {
			return "", "", errors.New("no GitHub token found")
		}
		out, errOut, err := runDoctorCaptureOpts(t, deps, doctorOpts{quiet: true})
		assert.Equal(t, 1, exitCode(err))
		assert.Empty(t, out)
		assert.Empty(t, errOut)
	})

	t.Run("json emits report only", func(t *testing.T) {
		out, errOut, err := runDoctorCaptureOpts(t, okDeps(), doctorOpts{asJSON: true, quiet: true})
		require.NoError(t, err)
		var rep doctorReport
		require.NoError(t, json.Unmarshal([]byte(out), &rep))
		assert.Equal(t, doctorReportVersion, rep.Version)
		assert.Empty(t, errOut)
	})
}

// TestRunDoctorNeverSpawnsGit is the charter guard: doctor resolves git identity
// by reading config files, never by exec'ing git. A fake `git` on PATH that
// records its invocation must never be called.
func TestRunDoctorNeverSpawnsGit(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "git-was-run")
	fakeGit := filepath.Join(dir, "git")
	script := "#!/bin/sh\ntouch " + sentinel + "\n"
	require.NoError(t, os.WriteFile(fakeGit, []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Real config loader (isolated to an empty temp global) so the identity check
	// actually exercises the file-reading path — the one place a naive impl might
	// have shelled out to git.
	isolateGitEnv(t, writeGlobalConfig(t, "[user]\n\tname = Ada\n\temail = ada@example.com\n"))
	deps := okDeps()
	deps.loadConfig = loadGitConfig

	_, _, err := runDoctorCapture(t, deps, false)
	require.NoError(t, err)
	assert.NoFileExists(t, sentinel, "doctor must never spawn git")
}

func TestSigningCheckVariants(t *testing.T) {
	on := gitConfig{values: map[string]string{"commit.gpgsign": "true", "user.signingkey": "K"}}
	assert.Equal(t, statusOK, signingCheck(on).Status)

	noKey := gitConfig{values: map[string]string{"commit.gpgsign": "true"}}
	assert.Equal(t, statusWarn, signingCheck(noKey).Status)

	off := gitConfig{values: map[string]string{}}
	assert.Equal(t, statusInfo, signingCheck(off).Status)
}

func TestSigningCheckAcceptsGitTruthyValues(t *testing.T) {
	// git treats yes/on/1 as true; a false negative here would wrongly report
	// signing as disabled.
	for _, v := range []string{"yes", "on", "1", "TRUE", "On"} {
		cfg := gitConfig{values: map[string]string{"commit.gpgsign": v, "user.signingkey": "K"}}
		assert.Equalf(t, statusOK, signingCheck(cfg).Status, "commit.gpgsign=%q should read as enabled", v)
	}
}

// TestScrubTokenEnvRemovesCredentials plants EVERY name in the canonical credential
// set, not the four that are obviously tokens. scrubTokenEnv derives its drop set
// from models.CredentialEnvVars, so a test that planted a subset would assert only
// the weaker half of the fact: it would keep passing while a variable added to the
// canonical list — a GitHub App identity, or GHORG_TOKEN_CMD, which ghorg runs as
// `sh -c` and uses the output of — rode into a probed child untouched.
//
// The names are literals for the reason the models test spells out: ranging over
// models.CredentialEnvVars() here would build the input from the same list the code
// reads, so deleting an entry would delete its own assertion. The Subset check is
// the other half — it fails loudly when the canonical list grows past what this test
// plants, which is the moment someone has to come back and add the literal.
func TestScrubTokenEnvRemovesCredentials(t *testing.T) {
	planted := []string{
		"GOLD_FINGER_PAT",
		"GITHUB_TOKEN",
		"GH_TOKEN",
		"GHORG_GITHUB_TOKEN",
		"GHORG_GITHUB_APP_ID",
		"GHORG_GITHUB_APP_INSTALLATION_ID",
		"GHORG_GITHUB_APP_PEM_PATH",
		"GHORG_GITHUB_TOKEN_FROM_GITHUB_APP",
		"GHORG_TOKEN_CMD",
	}
	assert.Subset(t, planted, models.CredentialEnvVars(),
		"every canonical credential variable must be planted here, or this test asserts only the ones it happens to know about")

	in := []string{"PATH=/usr/bin", "HOME=/home/ada"}
	for i, name := range planted {
		in = append(in, fmt.Sprintf("%s=secret-value-%02d", name, i))
	}

	joined := strings.Join(scrubTokenEnv(in), "\n")
	assert.Contains(t, joined, "PATH=/usr/bin")
	assert.Contains(t, joined, "HOME=/home/ada")
	for i, name := range planted {
		assert.NotContainsf(t, joined, fmt.Sprintf("secret-value-%02d", i),
			"%s must not survive scrubbing: a probed child that receives it can echo it", name)
	}
}

func TestGitIdentityCheckUnresolvedWarn(t *testing.T) {
	cfg := gitConfig{values: map[string]string{}, unresolved: true, reason: "include not evaluated"}
	c := gitIdentityCheck(cfg)
	assert.Equal(t, statusWarn, c.Status)
	assert.Contains(t, c.Detail, "include")
	assert.Contains(t, c.Detail, "no user.name/user.email")
}

// A hard parse/read error must NOT surface as a clean pass even when a name/email
// were parsed out of the (broken) config — git itself may reject it, so the value
// is unreliable. This is the false-pass codex flagged in pass 3.
func TestGitIdentityCheckParseErrorWarnsDespiteIdentity(t *testing.T) {
	cfg := gitConfig{
		values:     map[string]string{"user.name": "Ada", "user.email": "ada@example.com"},
		unresolved: true,
		parseError: true,
		reason:     "malformed config line in /etc/gitconfig",
	}
	c := gitIdentityCheck(cfg)
	assert.Equal(t, statusWarn, c.Status, "a malformed config must not read as a clean identity pass")
	assert.Contains(t, c.Detail, "could not be fully parsed")
	assert.NotContains(t, c.Detail, "include/includeIf was not evaluated",
		"the caveat must name the real cause (parse error), not misattribute it to an include")
}

// The signing check stays advisory, but its caveat must name a parse error
// distinctly from an unevaluated include so the operator isn't misled.
func TestSigningCheckParseErrorCaveat(t *testing.T) {
	cfg := gitConfig{
		values:     map[string]string{"commit.gpgsign": "true", "user.signingkey": "K"},
		unresolved: true,
		parseError: true,
		reason:     "malformed config line in /etc/gitconfig",
	}
	c := signingCheck(cfg)
	assert.Equal(t, statusWarn, c.Status, "a hard parse error must not read as [ok] signing, even for a machine consumer")
	assert.Contains(t, c.Detail, "could not be fully parsed")
	assert.NotContains(t, c.Detail, "include/includeIf was not evaluated")
}

func TestGitIdentityResolvedDespiteUnresolvedElsewhere(t *testing.T) {
	// If name+email are present, an unrelated unresolved section is still a clean
	// identity pass — but the detail must flag that an include could change it.
	cfg := gitConfig{
		values:     map[string]string{"user.name": "Ada", "user.email": "ada@example.com"},
		unresolved: true,
		reason:     "include not evaluated",
	}
	c := gitIdentityCheck(cfg)
	assert.Equal(t, statusOK, c.Status)
	assert.Contains(t, c.Detail, "include", "an unresolved include must be disclosed even on a pass")
}

func TestRunDoctorWarnsOnOldMultiGitter(t *testing.T) {
	// A multi-gitter below the known-good floor is an advisory warn, not a fail:
	// apply's signing and dry-run parsing were verified against the floor, so an
	// older one is a visible risk, but the tool may still work.
	deps := okDeps()
	deps.probeTool = func(_ context.Context, name string) (string, string, bool) {
		if name == "multi-gitter" {
			return "/usr/local/bin/multi-gitter", "multi-gitter version 0.60.0", true
		}
		return "/usr/local/bin/" + name, name + " v1.2.3", true
	}
	out, _, err := runDoctorCapture(t, deps, false)
	require.NoError(t, err, "an old tool version must not fail the run")
	assert.Contains(t, out, "[warn] multi-gitter")
	assert.Contains(t, out, "known-good floor")
}

func TestRunDoctorWarnsWhenMultiGitterVersionUnreadable(t *testing.T) {
	// A version string the probe can't parse must warn (can't verify the floor),
	// not silently pass as if it were known-good.
	deps := okDeps()
	deps.probeTool = func(_ context.Context, name string) (string, string, bool) {
		if name == "multi-gitter" {
			return "/usr/local/bin/multi-gitter", "", true
		}
		return "/usr/local/bin/" + name, name + " v1.2.3", true
	}
	out, _, err := runDoctorCapture(t, deps, false)
	require.NoError(t, err)
	assert.Contains(t, out, "[warn] multi-gitter")
	assert.Contains(t, out, "could not read a version")
}

func TestVersionFloorWarning(t *testing.T) {
	cases := []struct {
		version  string
		wantWarn bool
	}{
		{"multi-gitter version 0.63.1", false}, // exactly the floor is OK
		{"v0.63.2", false},
		{"1.0.0", false},
		{"v0.63.0", true}, // below floor
		{"0.62.9", true},
		{"garbage-no-version", true}, // unreadable
		{"", true},
	}
	for _, c := range cases {
		t.Run(c.version, func(t *testing.T) {
			_, _, warn := versionFloorWarning(c.version, multiGitterKnownGoodFloor)
			assert.Equal(t, c.wantWarn, warn)
		})
	}
}

// The quota check exists so an operator learns a fleet-scale run has no room
// BEFORE starting one, rather than partway through. Each case pins one of the
// answers it can give, and that the check itself never fails a run: doctor's fail
// means goldfinger cannot function, and a budget running low is not that — it is
// temporary, the check prints when it lifts, and the client waits out a limit it
// meets mid-run. A budget already spent does fail, but from the auth check, which
// is where every call including the login is refused; this check's job there is
// still to say when it lifts.
func TestRunDoctorQuotaCheck(t *testing.T) {
	reset := time.Date(2026, 8, 25, 14, 30, 0, 0, time.UTC)

	t.Run("ample quota reports the headroom", func(t *testing.T) {
		out, _, err := runDoctorCapture(t, okDeps(), false)
		require.NoError(t, err)
		assert.Contains(t, out, "[ok] rate-limit: 4900 of 5000 core API requests remaining")
	})

	t.Run("a nearly spent quota warns without failing the run", func(t *testing.T) {
		deps := okDeps()
		deps.readQuota = func(context.Context, string) (client.Quota, error) {
			return client.Quota{Limit: 5000, Remaining: quotaLowWater - 1, Reset: reset}, nil
		}
		out, _, err := runDoctorCapture(t, deps, false)

		require.NoError(t, err, "an exhausted quota is temporary — advisory, never a failed preflight")
		assert.Contains(t, out, "[warn] rate-limit: 99 of 5000 core API requests remaining")
		assert.Contains(t, out, "2026-08-25T14:30:00Z", "the operator needs the reset time to know when to rerun")
	})

	t.Run("a quota that cannot be read warns rather than inventing a number", func(t *testing.T) {
		deps := okDeps()
		deps.readQuota = func(context.Context, string) (client.Quota, error) {
			return client.Quota{}, errors.New("read API rate limit: 502 Bad Gateway")
		}
		out, _, err := runDoctorCapture(t, deps, false)

		require.NoError(t, err)
		assert.Contains(t, out, "[warn] rate-limit: could not read")
		assert.NotContains(t, out, "0 of 0", "an unreadable quota must not render as an exhausted one")
	})

	// A token that did not authenticate has no quota to report, and asking would
	// only restate the auth failure as a second one. The check is still emitted,
	// so the set of checks in --json is the same on every run.
	t.Run("a token that did not verify is reported as unchecked, and not asked about", func(t *testing.T) {
		deps := okDeps()
		deps.verifyLogin = func(context.Context, string) (string, error) {
			return "", errors.New("401 Bad credentials")
		}
		var asked bool
		deps.readQuota = func(context.Context, string) (client.Quota, error) {
			asked = true
			return client.Quota{}, nil
		}
		out, _, err := runDoctorCapture(t, deps, false)

		assert.Equal(t, 1, exitCode(err), "the auth failure still fails the run")
		assert.Contains(t, out, "[info] rate-limit: not checked")
		assert.False(t, asked, "there is nothing to ask about with a token that does not authenticate")
	})

	// The opposite case, and the likeliest reason doctor is being run at all: a
	// fleet run has just died on a rate limit and the question is when it lifts.
	// The login is refused along with everything else, so the naive reading is
	// "the token did not authenticate" — which would send an operator to rotate a
	// PAT that was never the problem, and suppress the one answer they came for.
	//
	// The quota endpoint is not billed against the limit blocking the rest, so it
	// still answers. Asking it is the whole point of the case.
	t.Run("a token GitHub rate limited is still asked for its reset, and not blamed", func(t *testing.T) {
		deps := okDeps()
		// Built with the response go-github renders the message from — a bare
		// literal panics in Error(), which fmt hides in the output and would
		// leave this passing on a fixture unlike anything production produces.
		limited := &github.RateLimitError{
			Message: "API rate limit exceeded",
			Rate:    github.Rate{Limit: 5000, Remaining: 0, Reset: github.Timestamp{Time: reset}},
			Response: &http.Response{
				StatusCode: http.StatusForbidden,
				Request:    &http.Request{Method: http.MethodGet, URL: &url.URL{Scheme: "https", Host: "api.github.com", Path: "/user"}},
			},
		}
		deps.verifyLogin = func(context.Context, string) (string, error) {
			return "", fmt.Errorf("authenticate with GOLD_FINGER_PAT: %w", limited)
		}
		deps.readQuota = func(context.Context, string) (client.Quota, error) {
			return client.Quota{Limit: 5000, Remaining: 0, Reset: reset}, nil
		}
		out, _, err := runDoctorCapture(t, deps, false)

		assert.Equal(t, 1, exitCode(err), "nothing works until the window rolls over, which is what fail means")
		assert.Contains(t, out, "rate limiting it, not rejecting it")
		assert.NotContains(t, out, "check the token is valid", "the token is not the problem and must not be blamed")
		assert.Contains(t, out, "[warn] rate-limit: 0 of 5000 core API requests remaining")
		assert.Contains(t, out, "2026-08-25T14:30:00Z", "when it lifts is the answer the operator came for")
	})
}

func TestDoctorReportContainsAllChecks(t *testing.T) {
	checks := gatherDoctorChecks(context.Background(), okDeps())
	names := make([]string, 0, len(checks))
	for _, c := range checks {
		names = append(names, c.Check)
	}
	joined := strings.Join(names, ",")
	for _, want := range []string{"auth", "auth-shadow", "rate-limit", "ghorg", "multi-gitter", "git-identity", "signing"} {
		assert.Contains(t, joined, want)
	}
}
