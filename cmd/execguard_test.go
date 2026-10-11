package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEveryExecSiteAppliesTheDelegateLifecycle is the drift guard for the defect
// this hardening fixed rather than for one instance of it.
//
// The history is the argument: the streaming CLI runners were written first, the
// MCP runners were added later and hardened themselves — own process group, bounded
// Wait, no inherited stdin — and the hardening never travelled back. Nothing failed;
// the two runners simply had different safety properties, and the CLI, which is the
// path an operator actually interrupts, had the weaker set. A fifth exec site added
// tomorrow would repeat it exactly, and the symptom (a delegate still cloning after
// goldfinger exited) surfaces on someone's machine, not in CI.
//
// So the rule is structural: every exec.CommandContext in production code must sit
// in a function that also calls setDelegateLifecycle, and no production code may
// start a child with plain exec.Command at all — a delegate with no context cannot
// be cancelled, so there is nothing for the lifecycle wiring to hang off.
//
// The setDelegateLifecycle half is deliberately blunt — it checks the call is made,
// not what it does — because the guards themselves are asserted behaviourally
// elsewhere (TestSetDelegateLifecycleAppliesAllThreeGuards, and the process-group
// reaping tests on both runners). What no behavioural test can catch is a NEW site
// that nobody wrote a test for.
func TestEveryExecSiteAppliesTheDelegateLifecycle(t *testing.T) {
	files := productionGoFiles(t)
	require.NotEmpty(t, files)

	checked := 0
	for _, path := range files {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		require.NoErrorf(t, err, "parse %s", path)

		// Resolve os/exec's name IN THIS FILE rather than assuming "exec": a file
		// that aliases the import would otherwise slip past the guard entirely, and
		// silently, because the four known sites keep the floor check below happy.
		pkg, imported := execImportName(file)
		if !imported {
			continue
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			assert.Falsef(t, callsFunc(fn.Body, pkg, "Command"),
				"%s: %s starts a child with exec.Command — with no context it cannot be cancelled at "+
					"all, so an interrupted run leaves it (and its git children) running; use "+
					"exec.CommandContext", path, fn.Name.Name)
			if !callsFunc(fn.Body, pkg, "CommandContext") {
				continue
			}
			checked++
			assert.Truef(t, callsFunc(fn.Body, "", "setDelegateLifecycle"),
				"%s: %s execs a child without setDelegateLifecycle — it would run in goldfinger's own "+
					"process group (so cancelling orphans its git grandchildren), with an unbounded Wait, "+
					"inheriting the operator's terminal on stdin. That wiring lives in package main, so a "+
					"site outside cmd/ has to move there or take an injected runner, as apply and mirror do",
				path, fn.Name.Name)
		}
	}

	// A rule that matched nothing would pass silently — most likely because the
	// runners moved out of this package.
	assert.GreaterOrEqual(t, checked, 4, "expected the four delegate/probe runners; found %d exec sites", checked)
}

// productionGoFiles returns every non-test .go file in the repository. The rule is
// repo-wide on purpose: exec lives in cmd today only because the runner is injected
// into apply/ and mirror/ as a func, and nothing but this test stops the next
// package from reaching for os/exec directly. Scanning only cmd/ would let exactly
// that land unnoticed.
func productionGoFiles(t *testing.T) []string {
	t.Helper()
	root := ".." // the package dir is one level down from the module root
	_, err := os.Stat(filepath.Join(root, "go.mod"))
	require.NoError(t, err, "expected the module root one level up; a moved package would silently narrow this guard")

	var files []string
	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != root && (strings.HasPrefix(name, ".") || name == "bin" || name == "vendor") {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			files = append(files, path)
		}
		return nil
	}))
	return files
}

// execImportName returns the name os/exec is bound to in this file, and whether it
// is callable at all. A dot-import yields "" — the same convention callsFunc uses
// for a bare identifier — and a blank import yields not-callable.
func execImportName(file *ast.File) (string, bool) {
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != "os/exec" {
			continue
		}
		if imp.Name == nil {
			return "exec", true
		}
		switch imp.Name.Name {
		case "_":
			return "", false
		case ".":
			return "", true
		default:
			return imp.Name.Name, true
		}
	}
	return "", false
}

// callsFunc reports whether body contains a call to pkg.name, or to a bare name
// when pkg is empty.
func callsFunc(body *ast.BlockStmt, pkg, name string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			ident, ok := fun.X.(*ast.Ident)
			if ok && pkg != "" && ident.Name == pkg && fun.Sel.Name == name {
				found = true
			}
		case *ast.Ident:
			if pkg == "" && fun.Name == name {
				found = true
			}
		}
		return !found
	})
	return found
}
