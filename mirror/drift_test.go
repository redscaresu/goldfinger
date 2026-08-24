package mirror

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// driftTestFile is this file's own name in both packages.
const driftTestFile = "drift_test.go"

// duplicatedDecls names every declaration that exists as a deliberate, identical
// copy in both apply/ and mirror/, with the file each copy lives in.
//
// The copies are not incidental helper code. overrideEnv is what maps the PAT
// onto each delegate's own token var, scrubs every other credential-bearing var,
// and de-duplicates keys (on Linux getenv returns the FIRST duplicate, so an
// ambient value would otherwise win). writeTempFile is half of what enforces the
// provable-same-set guarantee against ambient host config — the empty ghorgignore
// and empty ghorg --config in mirror, the key-occupying multi-gitter --config in
// apply. Between them they underwrite the two
// invariants AGENTS.md states as non-negotiable.
//
// Two copies of a security helper is the classic drift setup: a hardening applied
// to one silently does not reach the other, and today they agree only because
// someone has kept them in step by hand. This table is what stops that being luck.
var duplicatedDecls = []struct {
	name          string
	applyFile     string
	mirrorFile    string
	whyDuplicated string
}{
	{"overrideEnv", "apply.go", "mirror.go", "credential scrubbing and token mapping for the child environment"},
	{"writeTempFile", "apply.go", "mirror.go", "neutralising ambient host config so the lockfile is the exact set"},
	{"hasAnyPrefix", "apply.go", "mirror.go", "the prefix half of the scrub — it is what strips git's GIT_TRACE* family"},
	{"securityTest", "security_test.go", "security_test.go", "the security-invariant marker both packages' docs promise is identical"},
	{"assertGitEnvScrubbed", "security_test.go", "security_test.go", "the git-grandchild disclosure assertion, whose family list must not grow in one package only"},
	{"setGitHostileEnv", "security_test.go", "security_test.go", "the hostile environment that assertion runs against — it is worthless if the two drift apart"},
}

// TestDuplicatedDeclsHaveNotDrifted is the drift guard for the helpers apply/ and
// mirror/ each keep their own copy of. The copies stay where they are — extracting
// them would turn two package-private helpers into exported API and reopen the
// package-layout question — so the divergence risk is killed with a test instead.
// It lives in BOTH packages so whoever edits one sees the failure in the package
// they are editing, not somewhere they weren't looking. Keep the two copies of
// this file identical apart from the package clause.
func TestDuplicatedDeclsHaveNotDrifted(t *testing.T) {
	securityTest(t)
	for _, d := range duplicatedDecls {
		t.Run(d.name, func(t *testing.T) {
			inApply := declSource(t, filepath.Join("..", "apply", d.applyFile), d.name)
			inMirror := declSource(t, filepath.Join("..", "mirror", d.mirrorFile), d.name)
			assert.Equal(t, inApply, inMirror,
				"apply and mirror hold diverging copies of %s (%s) — a change to one must be applied to the other", d.name, d.whyDuplicated)
		})
	}
}

// TestDriftTestCopiesAreIdentical closes the obvious hole in the guard above: it
// only works if both packages run the same table, and the file holding that table
// is itself a duplicate. A divergence here would silently shrink what one package
// checks — the failure mode the guard exists to catch, one level up. Compared as
// raw bytes, modulo the package clause, since there is nothing in these two files
// that may legitimately differ.
func TestDriftTestCopiesAreIdentical(t *testing.T) {
	securityTest(t)
	const placeholder = "package <pkg>\n"
	inApply, err := os.ReadFile(filepath.Join("..", "apply", driftTestFile))
	require.NoError(t, err)
	inMirror, err := os.ReadFile(filepath.Join("..", "mirror", driftTestFile))
	require.NoError(t, err)
	assert.Equal(t,
		strings.Replace(string(inApply), "package apply\n", placeholder, 1),
		strings.Replace(string(inMirror), "package mirror\n", placeholder, 1),
		"apply/%s and mirror/%s must stay identical apart from the package clause", driftTestFile, driftTestFile)
}

// declSource returns the source of the named top-level func in path, normalised
// through go/printer so the comparison is about content rather than layout.
//
// Compared: the signature, the body, and every comment inside the declaration's
// own source range. Body comments count because some carry meaning a
// compiler-blind diff would miss — a `//nolint` directive suppresses a real
// linter finding, and an inline rationale is the record of why a hardening is
// safe; either appearing in one copy and not the other IS drift.
//
// Not compared: anything OUTSIDE that range — the doc comment above `func`, and
// any comment trailing the closing brace. The doc comment is the one part that
// legitimately differs (each copy names the delegate it serves: multi-gitter's
// --config versus ghorg's ghorgignore), and go/printer emits it from the decl's
// own Doc field rather than the comment list, so it is cleared below. The cost of
// that exemption is that a *declaration-level* `//nolint` — which sits in the doc
// group — is invisible here; that one is caught by `make lint` instead, since the
// unsuppressed copy still gets flagged.
func declSource(t *testing.T, path, name string) string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
	require.NoErrorf(t, err, "parse %s", path)
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != name {
			continue
		}
		// go/printer emits a declaration's own Doc field directly, independently of
		// the CommentedNode comment list, so clearing it is what actually exempts
		// the doc comment. The parsed file is this call's own throwaway AST.
		comments := commentsWithin(fn, file.Comments)
		fn.Doc = nil
		var b strings.Builder
		node := &printer.CommentedNode{Node: fn, Comments: comments}
		require.NoErrorf(t, printer.Fprint(&b, fset, node), "print %s from %s", name, path)
		return b.String()
	}
	t.Fatalf("no func %s in %s — update duplicatedDecls if it was renamed or removed", name, path)
	return ""
}

// commentsWithin returns the comment groups that sit inside fn's own source
// range. A doc comment precedes the `func` keyword and a trailing comment follows
// the closing brace, so both fall outside — see declSource for why that is the
// intended boundary.
func commentsWithin(fn *ast.FuncDecl, all []*ast.CommentGroup) []*ast.CommentGroup {
	var out []*ast.CommentGroup
	for _, cg := range all {
		if cg.Pos() >= fn.Pos() && cg.End() <= fn.End() {
			out = append(out, cg)
		}
	}
	return out
}
