package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ghTokenLookup resolves a token from the local GitHub CLI session. It is a var
// so tests can stub it; in production it points at the real gh CLI.
var ghTokenLookup = ghAuthToken

// resolveToken finds the GitHub token goldfinger uses for API discovery and
// hands to its child tools. Precedence:
//
//  1. GOLD_FINGER_PAT — explicit, and the path CI uses (a PAT stored as a secret).
//  2. the local GitHub CLI session (`gh auth token`) — so someone already
//     `gh auth login`'d needs no PAT at all; goldfinger rides their gh auth.
//
// It returns an error naming both options when neither yields a token.
func resolveToken() (string, error) {
	if t := strings.TrimSpace(os.Getenv(tokenEnvVar)); t != "" {
		return t, nil
	}
	if t, ok := ghTokenLookup(); ok {
		return t, nil
	}
	return "", fmt.Errorf("no GitHub token found: set %s (e.g. a PAT, as CI does), or run `gh auth login` so goldfinger can use your local GitHub CLI session", tokenEnvVar)
}

// ghAuthToken returns the token from the local gh CLI session, if gh is
// installed and logged in. A missing gh or a logged-out session is not an
// error here — it just means this fallback yielded nothing.
func ghAuthToken() (string, bool) {
	if _, err := exec.LookPath("gh"); err != nil {
		return "", false
	}
	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return "", false
	}
	t := strings.TrimSpace(string(out))
	return t, t != ""
}
