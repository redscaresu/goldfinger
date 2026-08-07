package mirror

import "testing"

// securityTest marks a test as one that locks a security invariant documented
// in SECURITY.md's "Auditing the source" audit map. It is a no-op at runtime;
// its only purpose is discoverability. See the fuller doc on the identical
// marker in apply/security_test.go (grep -rln 'securityTest(' --include='*_test.go'
// enumerates every security invariant test across packages). Keep the two
// definitions identical.
func securityTest(t testing.TB) { t.Helper() }
