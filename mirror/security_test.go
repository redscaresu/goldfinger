package mirror

import "testing"

// securityTest marks a test as one that locks a security invariant of
// goldfinger. It is a no-op at runtime; its only purpose is discoverability. See
// the fuller doc on the identical marker in apply/security_test.go
// (grep -rn 'securityTest(t)' --include='*_test.go' lists every security
// invariant test's call site across packages). Keep the two definitions
// identical.
func securityTest(t testing.TB) { t.Helper() }
