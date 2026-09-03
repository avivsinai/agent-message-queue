package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProductionDefaultRootCallsStayBehindErrorPreservingRegistration(t *testing.T) {
	// The process cwd is the secure temp root (TestMain, issue #707), so scan
	// the package source directory explicitly. A scan of cwd here would see
	// zero production files and pass this architecture guard vacuously.
	entries, err := os.ReadDir(cliTestPackageDir)
	if err != nil {
		t.Fatal(err)
	}
	files := token.NewFileSet()
	scanned := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		scanned++
		file, err := parser.ParseFile(files, filepath.Join(cliTestPackageDir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				identifier, ok := call.Fun.(*ast.Ident)
				if !ok || identifier.Name != "defaultRoot" {
					return true
				}
				if function.Name.Name != "registerImplicitRootFlag" {
					t.Errorf("%s: defaultRoot() bypasses error-preserving implicit-root registration", files.Position(call.Pos()))
				}
				return true
			})
		}
	}
	if scanned == 0 {
		t.Fatalf("scanned no production files under %s; the guard is vacuous", cliTestPackageDir)
	}
}
