package apply

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// securityTest marks a test (or fuzz target) as one that locks a security
// invariant documented in SECURITY.md's "Auditing the source" audit map. It is
// a no-op at runtime — its only purpose is discoverability, so an auditor can
// enumerate every security invariant test without trusting a curated list.
//
// List them all:
//
//	grep -rln 'securityTest(' --include='*_test.go'
//
// Run just them:
//
//	go test ./... -run 'ShellQuote|StripsSourcePAT|OverridesExistingToken|RefusesUnconfirmedLiveRun|RequiresValidSignMode|PinsEmptyConfig|Neutralises|PinsLayout|Invocation'
//
// The same one-line marker is defined in each package that holds security
// invariants (currently apply and mirror); keep the two definitions identical.
func securityTest(t testing.TB) { t.Helper() }

// FuzzShellQuote proves the central injection defence: no operator-supplied
// token can break out of the quoting that apply.writeScript applies before
// handing the command to multi-gitter. Rather than assert a property of the
// quoted *string*, it exercises the real assembly end to end — writeScript
// builds the actual `#!/bin/sh` script, a real POSIX sh runs it, and we confirm
// the fuzzed token reaches the program as exactly one argument, byte-for-byte,
// without merging into, displacing, or executing anything around it.
//
// The program under the script is /usr/bin/printf, invoked as
//
//	printf '%s\n' <fuzzed-token> SENTINEL
//
// so the output must be exactly "<token>\nSENTINEL\n". Any quote break-out would
// either corrupt the first line, drop/merge the SENTINEL word, or (the danger
// case) run an injected command — all of which fail the equality assertion.
//
// Seed payloads use only echo-based "injections": if quoting ever regressed the
// script would execute them, so they must stay harmless — never a destructive
// command.
func FuzzShellQuote(f *testing.F) {
	seeds := []string{
		"",
		"plain",
		"it's",
		"a b c",
		"a\tb",
		"a\nb",
		`$(echo pwned)`,
		"`echo pwned`",
		"; echo pwned",
		"&& echo pwned",
		"| echo pwned",
		"' ; echo pwned ; '",
		`'\''`,
		`\`,
		`"`,
		"*",
		"?",
		"~",
		"${HOME}",
		"--flag=value",
		"-rf",
		"newline\nand more",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, s string) {
		securityTest(t)
		// A NUL byte can't survive a POSIX argv (execve truncates at it), so it
		// isn't a realistic apply token; skip rather than assert on it.
		if strings.ContainsRune(s, 0) {
			t.Skip("NUL cannot appear in a POSIX argument list")
		}

		const sentinel = "SENTINEL"
		path, cleanup, err := writeScript([]string{"/usr/bin/printf", "%s\n", s, sentinel})
		require.NoError(t, err)
		defer cleanup()

		out, err := exec.CommandContext(context.Background(), "/bin/sh", path).CombinedOutput()
		require.NoErrorf(t, err, "script failed for %q: %s", s, out)
		assert.Equalf(t, s+"\n"+sentinel+"\n", string(out),
			"token %q did not round-trip as a single argument — possible quote break-out", s)
	})
}
