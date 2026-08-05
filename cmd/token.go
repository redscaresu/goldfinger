package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Human-readable labels for where a resolved token came from, surfaced to the
// user so it's obvious which credential a run is using.
const (
	tokenSourceEnv = "GOLD_FINGER_PAT"
	tokenSourceGh  = "local gh session (gh auth token)"
)

// ghAuthTimeout bounds the `gh auth token` call so a wedged gh (e.g. a stuck
// keychain prompt) can't hang goldfinger indefinitely.
const ghAuthTimeout = 5 * time.Second

// ghTokenLookup resolves a token from the local GitHub CLI session. It is a var
// so tests can stub it; in production it points at the real gh CLI.
var ghTokenLookup = ghAuthToken

// resolveToken finds the GitHub token goldfinger uses for API discovery and
// hands to its child tools, and reports which source it came from. Precedence:
//
//  1. GOLD_FINGER_PAT — explicit, and the path CI uses (a PAT stored as a secret).
//  2. the local GitHub CLI session (`gh auth token`) — so someone already
//     `gh auth login`'d needs no PAT at all; goldfinger rides their gh auth.
//
// It returns an error naming both options when neither yields a token.
func resolveToken(ctx context.Context) (token, source string, err error) {
	if t := strings.TrimSpace(os.Getenv(tokenEnvVar)); t != "" {
		return t, tokenSourceEnv, nil
	}
	if t, ok := ghTokenLookup(ctx); ok {
		return t, tokenSourceGh, nil
	}
	return "", "", fmt.Errorf("no GitHub token found: set %s (e.g. a PAT, as CI does), or run `gh auth login` so goldfinger can use your local GitHub CLI session", tokenEnvVar)
}

// ambientTokenVars are env vars that gh (and therefore goldfinger's `gh auth
// token` fallback) silently honours, shadowing a stored gh login. A stray one of
// these — common in CI or a shell rc — can make goldfinger authenticate as an
// unexpected identity, which then surfaces as a confusing empty/partial result
// rather than an obvious auth error. GOLD_FINGER_PAT is deliberately not here: it
// is goldfinger's own explicit, documented input, not a stray shadow.
var ambientTokenVars = []string{"GITHUB_TOKEN", "GH_TOKEN"}

// announceTokenSource tells the user which credential the run is using, so the
// common question "is it using my gh session?" is answered without guesswork. It
// also warns when a stray ambient token may be shadowing that credential.
func announceTokenSource(w io.Writer, source string) {
	fmt.Fprintf(w, "auth: using %s\n", source)
	if warn := ambientTokenWarning(source); warn != "" {
		fmt.Fprintln(w, warn)
	}
}

// ambientTokenWarning returns a warning when goldfinger resolved its token from
// the local gh session AND an ambient GITHUB_TOKEN/GH_TOKEN is set — because
// `gh auth token` may then be returning that ambient token instead of the stored
// login, so goldfinger could be acting as an unexpected identity. It returns ""
// when the token came from GOLD_FINGER_PAT (the ambient var is irrelevant to
// goldfinger's own resolution there) or when no ambient var is set.
func ambientTokenWarning(source string) string {
	if source != tokenSourceGh {
		return ""
	}
	var present []string
	for _, v := range ambientTokenVars {
		if strings.TrimSpace(os.Getenv(v)) != "" {
			present = append(present, v)
		}
	}
	if len(present) == 0 {
		return ""
	}
	return fmt.Sprintf("auth: warning: %s set in the environment — `gh auth token` may be using it instead of your stored gh login, so goldfinger could be authenticating as an unexpected identity. If discovery resolves the wrong repos (or none), unset it or set %s explicitly.",
		strings.Join(present, "/"), tokenEnvVar)
}

// announcePrincipal reports the authenticated GitHub login a run resolved to, so
// an operator (or agent) can see *which identity* is in play — the other half of
// diagnosing a wrong-token result. The token value itself is never printed.
func announcePrincipal(w io.Writer, login string) {
	if login == "" {
		return
	}
	fmt.Fprintf(w, "auth: authenticated as %s\n", login)
}

// verifyAndAnnouncePrincipal makes the one read-only /user call needed to resolve
// and print the authenticated principal, for commands (mirror, apply) that
// otherwise delegate straight to a child tool. It exists so those runs honour the
// documented "every run prints its principal" contract and fail fast on a bad
// token *before* a long ghorg clone or a fleet-wide multi-gitter run — without
// re-running discovery (this is an identity check, not a repo-set query). It
// returns the verify error so the caller can abort with a clear message.
func verifyAndAnnouncePrincipal(ctx context.Context, errOut io.Writer, token string) error {
	login, err := verifyLoginWithClient(ctx, token)
	if err != nil {
		return fmt.Errorf("verifying token: %w", err)
	}
	announcePrincipal(errOut, login)
	return nil
}

// ghAuthToken returns the token from the local gh CLI session, if gh is
// installed and logged in. A missing gh, a logged-out session, or a timeout is
// not an error here — it just means this fallback yielded nothing.
func ghAuthToken(ctx context.Context) (string, bool) {
	if _, err := exec.LookPath("gh"); err != nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(ctx, ghAuthTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err != nil {
		return "", false
	}
	t := strings.TrimSpace(string(out))
	return t, t != ""
}
