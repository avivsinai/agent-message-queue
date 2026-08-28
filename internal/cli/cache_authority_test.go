package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDefaultCachePathIsSolePlatformCacheAuthority(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(testFile), "..", ".."))
	updateDir := filepath.Join(repoRoot, "internal", "update")
	var offenders []string
	err := filepath.WalkDir(repoRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path == filepath.Join(repoRoot, ".git") || path == filepath.Join(repoRoot, ".agent-mail") || path == updateDir {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		osName := ""
		for _, imported := range file.Imports {
			if strings.Trim(imported.Path.Value, `"`) != "os" {
				continue
			}
			osName = "os"
			if imported.Name != nil {
				osName = imported.Name.Name
			}
		}
		if osName == "" || osName == "." {
			return nil
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "UserCacheDir" {
				return true
			}
			identifier, ok := selector.X.(*ast.Ident)
			if ok && identifier.Name == osName {
				offenders = append(offenders, path)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) != 0 {
		t.Fatalf("os.UserCacheDir is called outside internal/update; use update.DefaultCachePath: %s", strings.Join(offenders, ", "))
	}
}
