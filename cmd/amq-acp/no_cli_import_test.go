package main

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

func TestACPSourcesDoNotImportCLI(t *testing.T) {
	root := repoRoot()
	dirs := []string{
		filepath.Join(root, "cmd", "amq-acp"),
		filepath.Join(root, "internal", "acp"),
	}
	fset := token.NewFileSet()
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			for _, spec := range file.Imports {
				imp := strings.Trim(spec.Path.Value, `"`)
				if strings.Contains(imp, "/internal/cli") {
					t.Errorf("%s imports %s; ACP workers must not drain via the CLI", path, imp)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
