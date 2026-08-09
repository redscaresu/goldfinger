package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/redscaresu/goldfinger/client"
	"github.com/spf13/cobra"
)

// doctor check statuses. ok/info never fail the run; warn is advisory; fail means
// goldfinger cannot function for at least one command and drives a non-zero exit.
const (
	statusOK   = "ok"
	statusInfo = "info"
	statusWarn = "warn"
	statusFail = "fail"
)

// doctorProbeTimeout bounds each child-tool `version` probe so a wedged binary
// can't hang the preflight.
const doctorProbeTimeout = 5 * time.Second

// doctorCheck is one preflight result. Fix is a concrete next action, empty when
// the check passed cleanly.
type doctorCheck struct {
	Check  string `json:"check"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

// doctorReport is the --json payload for doctor (issue #27 §1): a versioned list
// of checks. It carries no secrets — the token value is never included, only its
// source and the principal it resolves to.
type doctorReport struct {
	Version int           `json:"version"`
	Checks  []doctorCheck `json:"checks"`
}

// doctorDeps are doctor's injectable side-effects, so runDoctor is testable
// without a network, real child tools, or the host's git config. Production wiring
// lives in newDoctorCmd.
type doctorDeps struct {
	resolveToken func(ctx context.Context) (token, source string, err error)
	verifyLogin  func(ctx context.Context, token string) (login string, err error)
	probeTool    func(ctx context.Context, name string) (path, version string, ok bool)
	loadConfig   func() gitConfig
}

// doctorOpts groups runDoctor's non-dependency inputs.
type doctorOpts struct {
	asJSON bool
	quiet  bool
}

func newDoctorCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Run read-only preflight checks (auth, child tools, git identity, signing)",
		Long: "doctor reports whether goldfinger's environment is ready: which token " +
			"source and GitHub principal a run would use (and whether an ambient token " +
			"may be shadowing it), whether ghorg and multi-gitter are on PATH, and " +
			"whether a git identity and commit signing are configured for apply.\n\n" +
			"It is entirely read-only — it never writes to GitHub, never runs git, and " +
			"never prints the token. Exit status is 0 when nothing failed, 1 when any " +
			"check failed (a missing token or child tool), and 2 if doctor itself " +
			"could not run.",
		RunE: func(cmd *cobra.Command, args []string) error {
			deps := doctorDeps{
				resolveToken: resolveToken,
				verifyLogin:  verifyLoginWithClient,
				probeTool:    probeToolDefault,
				loadConfig:   loadGitConfig,
			}
			return runDoctor(cmd.Context(), deps, doctorOpts{asJSON: asJSON, quiet: quietRequested(cmd)}, cmd.OutOrStdout(), humanErr(cmd))
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false,
		"emit the checks as JSON on stdout instead of the human report; exit code is unchanged (0 clean, 1 a failed check, 2 doctor error)")
	return cmd
}

// runDoctor gathers every preflight check and reports them. It returns
// exitError{code:1} when any check failed so the process exits non-zero for CI,
// without printing an error (the report already carries the detail). A genuine
// inability to emit the report (a broken stdout) surfaces as a plain error → exit
// 2, matching the exit-code contract.
func runDoctor(ctx context.Context, deps doctorDeps, o doctorOpts, out, errOut io.Writer) error {
	errOut = quietWriter(errOut, o.quiet)
	checks := gatherDoctorChecks(ctx, deps)

	if o.asJSON {
		if err := emitJSON(out, doctorReport{Version: doctorReportVersion, Checks: checks}, o.quiet); err != nil {
			return err
		}
	} else if !o.quiet {
		renderDoctor(out, errOut, checks)
	}

	for _, c := range checks {
		if c.Status == statusFail {
			return exitError{code: 1}
		}
	}
	return nil
}

// gatherDoctorChecks runs each check in a fixed order (auth first, since a wrong
// identity is the most common and most confusing failure) and returns the flat
// list.
func gatherDoctorChecks(ctx context.Context, deps doctorDeps) []doctorCheck {
	var checks []doctorCheck
	checks = append(checks, authChecks(ctx, deps)...)
	checks = append(checks,
		toolCheck(ctx, deps, "ghorg", "https://github.com/gabrie30/ghorg#installation"),
		toolCheck(ctx, deps, "multi-gitter", "https://github.com/lindell/multi-gitter#installation"),
	)
	cfg := deps.loadConfig()
	checks = append(checks, gitIdentityCheck(cfg), signingCheck(cfg))
	return checks
}

// authChecks resolves the token (never printing it), verifies the principal, and
// flags a possible ambient-token shadow. A token that cannot be resolved is a
// hard fail — nothing works without it.
func authChecks(ctx context.Context, deps doctorDeps) []doctorCheck {
	token, source, err := deps.resolveToken(ctx)
	if err != nil {
		return []doctorCheck{{
			Check:  "auth",
			Status: statusFail,
			Detail: "no GitHub token resolved",
			Fix:    "set GOLD_FINGER_PAT to a PAT, or run `gh auth login` so goldfinger can use your gh session",
		}}
	}

	var checks []doctorCheck
	login, verr := deps.verifyLogin(ctx, token)
	if verr != nil {
		checks = append(checks, doctorCheck{
			Check:  "auth",
			Status: statusFail,
			Detail: fmt.Sprintf("token from %s did not verify: %v", source, verr),
			Fix:    "check the token is valid and has repo scope",
		})
	} else {
		checks = append(checks, doctorCheck{
			Check:  "auth",
			Status: statusOK,
			Detail: fmt.Sprintf("authenticated as %s (via %s)", login, source),
		})
	}

	if ambientTokenWarning(source) != "" {
		checks = append(checks, doctorCheck{
			Check:  "auth-shadow",
			Status: statusWarn,
			Detail: "ambient GITHUB_TOKEN/GH_TOKEN is set — `gh auth token` may be returning it instead of your stored gh login, so goldfinger could authenticate as an unexpected identity",
			Fix:    "unset GITHUB_TOKEN GH_TOKEN, or set GOLD_FINGER_PAT explicitly",
		})
	} else {
		checks = append(checks, doctorCheck{
			Check:  "auth-shadow",
			Status: statusOK,
			Detail: "no ambient token shadowing detected",
		})
	}
	return checks
}

// toolCheck reports whether a delegated child tool is on PATH, with its version
// when it can be probed. A missing tool is a fail for the command that needs it;
// doctor reports both so the operator sees the whole picture in one run.
func toolCheck(ctx context.Context, deps doctorDeps, name, installHint string) doctorCheck {
	path, version, ok := deps.probeTool(ctx, name)
	if !ok {
		return doctorCheck{
			Check:  name,
			Status: statusFail,
			Detail: name + " not found on PATH",
			Fix:    "install it: " + installHint,
		}
	}
	detail := path
	if version != "" {
		detail = fmt.Sprintf("%s (%s)", version, path)
	}
	return doctorCheck{Check: name, Status: statusOK, Detail: detail}
}

// gitIdentityCheck reports whether a committing identity is configured. A present
// user.name+user.email is a clean pass even when an unevaluated include leaves some
// uncertainty (that only annotates the detail) — but a hard parse/read error is
// different: git itself may reject the config, so we never report a pass on top of
// one. A missing identity is a warn, not a fail: mirror needs none, and only apply
// is affected — where multi-gitter would silently make no commit.
func gitIdentityCheck(cfg gitConfig) doctorCheck {
	name, _ := cfg.get("user.name")
	email, _ := cfg.get("user.email")
	hasIdentity := strings.TrimSpace(name) != "" && strings.TrimSpace(email) != ""

	// A hard parse/read problem outranks anything we think we read: git may not be
	// able to load this config at all, so an identity we parsed out of it can't be
	// trusted. Warn regardless of whether we saw a name/email.
	if cfg.parseError {
		return doctorCheck{
			Check:  "git-identity",
			Status: statusWarn,
			Detail: "git config could not be fully parsed (" + cfg.reason + ") — git itself may reject it, so any identity read from it is unreliable",
			Fix:    "fix the git config, then confirm with `git config --get user.name` / `git config --get user.email`",
		}
	}

	if hasIdentity {
		detail := fmt.Sprintf("%s <%s>", strings.TrimSpace(name), strings.TrimSpace(email))
		if cfg.unresolved {
			// Identity is present, so apply will commit — a clean pass. But an
			// unevaluated include/includeIf could override the value shown; say so
			// rather than warn (multi-gitter checks out under its own temp dir, where
			// a gitdir-scoped includeIf usually won't match anyway).
			detail += " (an include/includeIf was not evaluated and may change this)"
		}
		return doctorCheck{Check: "git-identity", Status: statusOK, Detail: detail}
	}
	if cfg.unresolved {
		return doctorCheck{
			Check:  "git-identity",
			Status: statusWarn,
			Detail: "an include/includeIf was not evaluated (" + cfg.reason + ") and no user.name/user.email was found in the parts read",
			Fix:    "confirm with `git config --get user.name` / `git config --get user.email`",
		}
	}
	return doctorCheck{
		Check:  "git-identity",
		Status: statusWarn,
		Detail: "git user.name/user.email not set — multi-gitter apply would make no commit and open no PR",
		Fix:    "git config --global user.name '...' && git config --global user.email '...'",
	}
}

// signingCheck reports commit-signing readiness for `--sign local`. It is
// advisory only — never a fail — because the operator picks the signing mode per
// apply, and `--sign github`/`--sign none` don't depend on local git config.
func signingCheck(cfg gitConfig) doctorCheck {
	gpgsign, _ := cfg.get("commit.gpgsign")
	key, hasKey := cfg.get("user.signingkey")
	signOn := gitBool(gpgsign)
	var c doctorCheck
	switch {
	case signOn && hasKey && strings.TrimSpace(key) != "":
		c = doctorCheck{
			Check:  "signing",
			Status: statusOK,
			Detail: "commit.gpgsign is on with user.signingkey set — `--sign local` will sign; ensure the public key is uploaded to GitHub and gpg-agent is warm",
		}
	case signOn:
		c = doctorCheck{
			Check:  "signing",
			Status: statusWarn,
			Detail: "commit.gpgsign is on but no user.signingkey — git will pick a default key; `--sign local` may fail if none matches",
			Fix:    "set user.signingkey, or use --sign github",
		}
	default:
		c = doctorCheck{
			Check:  "signing",
			Status: statusInfo,
			Detail: "commit.gpgsign not enabled — `--sign local` relies on your git config; `--sign github` signs via GitHub's key, `--sign none` is unsigned",
		}
	}
	// Config uncertainty could change any of these conclusions (enable/disable
	// signing, or set/override the key), so disclose it whatever the branch —
	// mirroring gitIdentityCheck's honesty. A hard parse error is a stronger caveat
	// than an unevaluated include: git may reject the config, so a machine consumer
	// reading the status field must not see [ok]. Force warn (still advisory,
	// never a fail) and word the caveat by cause.
	switch {
	case cfg.parseError:
		c.Status = statusWarn
		c.Detail += " (git config could not be fully parsed: " + cfg.reason + " — git may reject it, so this reading is unreliable)"
	case cfg.unresolved:
		c.Detail += " (an include/includeIf was not evaluated and may change this)"
	}
	return c
}

// renderDoctor writes the human report: a banner to stderr, the check lines to
// stdout (so the report is pipeable, matching check's stdout=data convention).
func renderDoctor(out, errOut io.Writer, checks []doctorCheck) {
	banner(errOut, "goldfinger doctor")
	s := newStyler(out)
	for _, c := range checks {
		fmt.Fprintf(out, "%s %s: %s\n", s.paint(statusColor(c.Status), "["+c.Status+"]"), c.Check, c.Detail)
		if c.Fix != "" {
			fmt.Fprintf(out, "       fix: %s\n", c.Fix)
		}
	}
}

// statusColor maps a status to an ANSI code for the human report.
func statusColor(status string) string {
	switch status {
	case statusOK:
		return cGreen
	case statusFail:
		return cRed
	case statusWarn:
		return cYellow
	default:
		return cCyan
	}
}

// verifyLoginWithClient is the production principal check: it builds an API client
// from the token and returns the authenticated login. It is only reached with a
// non-empty token (authChecks resolves the token first).
func verifyLoginWithClient(ctx context.Context, token string) (string, error) {
	c, err := client.New(token)
	if err != nil {
		return "", err
	}
	return c.Verify(ctx)
}

// probeToolDefault reports whether name is on PATH and, best-effort, its version.
// A failed version probe is not fatal — the tool is still usable — so ok tracks
// PATH presence alone.
func probeToolDefault(ctx context.Context, name string) (path, version string, ok bool) {
	p, err := exec.LookPath(name)
	if err != nil {
		return "", "", false
	}
	// LookPath can return a relative path when PATH has relative entries; make it
	// absolute so the reported path is unambiguous and the probe runs that exact
	// binary rather than re-resolving the name.
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return p, probeToolVersion(ctx, p), true
}

// probeToolVersion runs `<path> version` with a short timeout and returns its
// first output line trimmed, or "" if the probe fails. Both ghorg and
// multi-gitter expose a `version` subcommand.
//
// The child's environment is scrubbed of every token var goldfinger might hold
// (GOLD_FINGER_PAT and the GITHUB_TOKEN/GH_TOKEN/GHORG_GITHUB_TOKEN family): a
// version probe needs no credential, and a rogue or wrong binary on PATH must not
// be able to echo a token that goldfinger would then print. The charter forbids
// ever printing the token — this keeps that true even for a hostile PATH entry.
func probeToolVersion(ctx context.Context, path string) string {
	ctx, cancel := context.WithTimeout(ctx, doctorProbeTimeout)
	defer cancel()
	c := exec.CommandContext(ctx, path, "version")
	c.Env = scrubTokenEnv(os.Environ())
	out, err := c.Output()
	if err != nil {
		return ""
	}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	if sc.Scan() {
		return strings.TrimSpace(sc.Text())
	}
	return ""
}

// scrubTokenEnv returns env with every credential-bearing variable removed, so a
// probed child can never receive (and therefore never echo) goldfinger's token.
func scrubTokenEnv(env []string) []string {
	drop := map[string]bool{
		tokenEnvVar:          true, // GOLD_FINGER_PAT
		"GITHUB_TOKEN":       true,
		"GH_TOKEN":           true,
		"GHORG_GITHUB_TOKEN": true,
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		if drop[name] {
			continue
		}
		out = append(out, kv)
	}
	return out
}
