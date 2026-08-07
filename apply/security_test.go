package apply

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// securityTest marks a test (or fuzz target) as one that locks a security
// invariant of goldfinger — the same invariants enumerated in SECURITY.md's
// audit map. It is a no-op at runtime; its only purpose is discoverability, so
// an auditor can list every security invariant test without trusting a curated
// list:
//
//	grep -rln 'securityTest(' --include='*_test.go'
//
// Run just them:
//
//	go test ./... -run 'ShellQuote|StripsSourcePAT|OverridesExistingToken|RefusesUnconfirmedLiveRun|RequiresValidSignMode|SignMode|LocalSign|PinsEmptyConfig|Neutralises|PinsLayout|Invocation'
//
// The same one-line marker is defined in each package that holds security
// invariants (currently apply and mirror); keep the two definitions identical.
func securityTest(t testing.TB) { t.Helper() }

// maxFuzzToken caps the token length FuzzShellQuote will execute. The quoting
// logic is length-independent, so a few KiB exercises it fully; the cap keeps a
// very long fuzz string from hitting the OS argv limit (ARG_MAX/E2BIG) and
// failing the exec for a reason unrelated to quoting — a false failure.
const maxFuzzToken = 4096

// FuzzShellQuote proves the central injection defence: no operator-supplied
// token can break out of the quoting apply.writeScript applies before handing
// the command to multi-gitter. Rather than assert a property of the quoted
// *string*, it exercises the real assembly end to end — writeScript builds the
// actual `#!/bin/sh` script, a real POSIX sh runs it, and we confirm the fuzzed
// token reaches the program as exactly one argument, byte-for-byte, without
// merging into, displacing, or executing anything around it.
//
// The program under the script is /usr/bin/printf, invoked as
//
//	printf '%s\n' <fuzzed-token> SENTINEL
//
// so the output must be exactly "<token>\nSENTINEL\n". Any quote break-out would
// either corrupt the first line, drop/merge the SENTINEL word, or (the danger
// case) run an injected command — all of which fail the equality assertion.
//
// This is an argv-round-trip oracle under a real shell, not a full proof that no
// shell side effect ran: it observes stdout and exit status, so a *silent*
// command substitution wouldn't be seen directly. That's an accepted limit —
// the current shellQuote can't produce one, and any regression that let a
// substitution escape would also corrupt the round-trip and fail here.
//
// Blast-radius containment: `go test` (including CI) only ever executes the
// curated seed corpus below — all harmless echo-based payloads. Active fuzzing
// (`-fuzz`) generates unconstrained mutations, so IF shellQuote ever regressed a
// mutation could get a command substitution past the quotes. To keep that from
// touching anything real, each script runs with an emptied environment (`PATH=`
// so bareword commands don't resolve) and both HOME and the working directory
// pointed at a throwaway temp dir, under a hard timeout so a hang can't wedge the
// run. Absolute-path payloads remain theoretically possible after such a
// regression, so run active fuzzing in a disposable environment.
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
		if len(s) > maxFuzzToken {
			t.Skip("token longer than the argv limit is out of scope for quote parsing")
		}

		const sentinel = "SENTINEL"
		path, cleanup, err := writeScript([]string{"/usr/bin/printf", "%s\n", s, sentinel})
		require.NoError(t, err)
		defer cleanup()

		// Contain the blast radius if a future shellQuote regression let a fuzzed
		// input escape: no PATH (bareword commands don't resolve), HOME and CWD in
		// a throwaway dir, and a hard timeout against hangs.
		sandbox := t.TempDir()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/bin/sh", path)
		cmd.Dir = sandbox
		cmd.Env = []string{"PATH=", "HOME=" + sandbox}

		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "script failed for %q: %s", s, out)
		assert.Equalf(t, s+"\n"+sentinel+"\n", string(out),
			"token %q did not round-trip as a single argument — possible quote break-out", s)
	})
}
