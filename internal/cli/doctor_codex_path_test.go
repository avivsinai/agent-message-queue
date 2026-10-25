package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The installed 0.111.0 sweep found amx selecting an older Codex before a
// working installation, with repeated PATH entries and a symlinked binary.
func TestDoctorNamesShadowedCodexExecutables(t *testing.T) {
	root := t.TempDir()
	first, second, linked := filepath.Join(root, "first"), filepath.Join(root, "second"), filepath.Join(root, "linked")
	name := "codex"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	for _, dir := range []string{first, second, linked} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{first, second} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("not a program: doctor must not execute this file"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// A hard link also works on Windows without symlink privileges.
	if err := os.Link(filepath.Join(first, name), filepath.Join(linked, name)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", strings.Join([]string{first, first, linked, second}, string(os.PathListSeparator)))
	check, ok := checkCodexExecutablePaths()
	if !ok || check.Status != "warn" || !strings.Contains(check.Message, "select "+filepath.Join(first, name)) || !strings.Contains(check.Message, "alternatives: "+filepath.Join(second, name)) {
		t.Fatalf("check = %#v, %v", check, ok)
	}
	if strings.Contains(check.Message, linked) || strings.Count(check.Message, filepath.Join(first, name)) != 1 {
		t.Fatalf("same executable reported more than once: %s", check.Message)
	}
}
