package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestScrubTokenEnvRemovesCredentials(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		tokenEnvVar + "=secret1",
		"GITHUB_TOKEN=secret2",
		"GH_TOKEN=secret3",
		"GHORG_GITHUB_TOKEN=secret4",
		"HOME=/home/ada",
	}
	out := scrubTokenEnv(in)
	joined := strings.Join(out, "\n")
	assert.Contains(t, joined, "PATH=/usr/bin")
	assert.Contains(t, joined, "HOME=/home/ada")
	for _, secret := range []string{"secret1", "secret2", "secret3", "secret4"} {
		assert.NotContains(t, joined, secret, "no token value may survive scrubbing")
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

func TestDoctorReportContainsAllChecks(t *testing.T) {
	checks := gatherDoctorChecks(context.Background(), okDeps())
	names := make([]string, 0, len(checks))
	for _, c := range checks {
		names = append(names, c.Check)
	}
	joined := strings.Join(names, ",")
	for _, want := range []string{"auth", "auth-shadow", "ghorg", "multi-gitter", "git-identity", "signing"} {
		assert.Contains(t, joined, want)
	}
}
