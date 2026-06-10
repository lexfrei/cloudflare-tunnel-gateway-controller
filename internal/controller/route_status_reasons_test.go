package controller

// Guard for the spec recommendation that route status conditions use only
// the standard RouteReason* vocabulary (Accepted and ResolvedRefs reasons).
// All condition reasons in the status-feeding packages must come from the
// vendored Gateway API constants -- a raw string literal assigned to a
// Reason field or reason variable is exactly how a custom reason would
// sneak in, so the guard fails on any such literal.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// reasonSourceDirs are the packages that feed Reason values into route
// status conditions: the status writers themselves, the binding validator,
// the ingress backend resolver, and the proxy converter diagnostics.
var reasonSourceDirs = []string{
	"internal/controller",
	"internal/routebinding",
	"internal/ingress",
	"internal/proxy",
}

// ownCRDReasonFiles write conditions on this project's own CRDs
// (GatewayClassConfig), where the Gateway API reason vocabulary does not
// apply -- their custom reasons are the API contract of that CRD, not a
// deviation from the Gateway API spec.
var ownCRDReasonFiles = map[string]bool{
	"gatewayclassconfig_controller.go": true,
}

func TestRouteStatusReasons_NoCustomReasonLiterals(t *testing.T) {
	t.Parallel()

	repoRoot := repoRootFromWD(t)

	for _, dir := range reasonSourceDirs {
		absDir := filepath.Join(repoRoot, dir)

		entries, err := os.ReadDir(absDir)
		if err != nil {
			t.Fatalf("read dir %s: %v", dir, err)
		}

		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}

			if ownCRDReasonFiles[name] {
				continue
			}

			checkFileForReasonLiterals(t, dir, filepath.Join(absDir, name))
		}
	}
}

// checkFileForReasonLiterals parses one source file and reports every string
// literal assigned to a Reason field or reason variable.
func checkFileForReasonLiterals(t *testing.T, relDir, absPath string) {
	t.Helper()

	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, absPath, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", absPath, err)
	}

	report := func(pos token.Pos, value string) {
		position := fset.Position(pos)
		t.Errorf("%s/%s:%d assigns raw string literal %q to a Reason; condition reasons must use the standard gatewayv1.RouteReason* / spec constants",
			relDir, filepath.Base(absPath), position.Line, value)
	}

	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.KeyValueExpr:
			key, ok := node.Key.(*ast.Ident)
			if !ok || key.Name != "Reason" {
				return true
			}

			if lit, isLiteral := unwrapStringLiteral(node.Value); isLiteral && lit != `""` {
				report(node.Pos(), lit)
			}
		case *ast.AssignStmt:
			for i, lhs := range node.Lhs {
				ident := reasonIdent(lhs)
				if ident == "" || i >= len(node.Rhs) {
					continue
				}

				if lit, isLiteral := unwrapStringLiteral(node.Rhs[i]); isLiteral && lit != `""` {
					report(node.Pos(), lit)
				}
			}
		}

		return true
	})
}

// reasonIdent returns the identifier name when the expression is a plain
// `reason` variable or a `<x>.Reason` selector, otherwise "".
func reasonIdent(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		if e.Name == "reason" || e.Name == "Reason" {
			return e.Name
		}
	case *ast.SelectorExpr:
		if e.Sel.Name == "Reason" {
			return e.Sel.Name
		}
	}

	return ""
}

// unwrapStringLiteral reports whether the expression is a basic string
// literal, unwrapping a single string(...) conversion if present.
func unwrapStringLiteral(expr ast.Expr) (string, bool) {
	if call, ok := expr.(*ast.CallExpr); ok && len(call.Args) == 1 {
		if fn, isIdent := call.Fun.(*ast.Ident); isIdent && fn.Name == "string" {
			expr = call.Args[0]
		}
	}

	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}

	return lit.Value, true
}

// repoRootFromWD walks up from the working directory to the go.mod root.
func repoRootFromWD(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above working directory")
		}

		dir = parent
	}
}
