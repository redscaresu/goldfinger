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

// announceTokenSource tells the user which credential the run is using, so the
// common question "is it using my gh session?" is answered without guesswork.
func announceTokenSource(w io.Writer, source string) {
	fmt.Fprintf(w, "auth: using %s\n", source)
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
