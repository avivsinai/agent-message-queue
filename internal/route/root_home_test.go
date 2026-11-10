package route

import (
	"os"
	"path/filepath"
	"testing"
)

// A relative root outside Git is not resolved to a queue in the home
// directory above the project (audit 2026-11-10).
func TestFindRootInParentsStopsBeforeHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	proj := filepath.Join(home, "proj")
	for _, d := range []string{filepath.Join(home, ".agent-mail"), filepath.Join(proj, "sub")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if found, ok := (Walker{}).FindRootInParents(filepath.Join(proj, "sub"), ".agent-mail"); ok {
		t.Fatalf("found %s above the project, want none", found)
	}
	if err := os.MkdirAll(filepath.Join(proj, ".agent-mail"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, ok := (Walker{}).FindRootInParents(filepath.Join(proj, "sub"), ".agent-mail"); !ok {
		t.Fatal("project queue below home not found")
	}
}
