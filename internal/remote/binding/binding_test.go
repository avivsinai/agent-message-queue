package binding

import (
	"os"
	"path/filepath"
	"testing"
)

// Codex #885 P2 #3: an off for session A must not remove a binding that a
// concurrent attach already moved to session B.
func TestRemoveLeavesAnotherSessionsBinding(t *testing.T) {
	t.Setenv(EnvPath, filepath.Join(t.TempDir(), "remote", "binding.json"))
	a := Binding{Root: "/r", Target: "claude:1", NativeSession: "s-a"}
	b := Binding{Root: "/r", Target: "claude:1", NativeSession: "s-b"}
	if err := Write(b); err != nil {
		t.Fatal(err)
	}
	if removed, err := Remove(a.Same); err != nil || removed {
		t.Fatalf("removed=%v err=%v; A's off removed B's binding", removed, err)
	}
	if got, err := Read(); err != nil || !got.Same(b) {
		t.Fatalf("binding = %+v %v", got, err)
	}
}

// Codex #885 P2 #4: a symlinked binding directory was followed.
func TestSymlinkedBindingDirectoryIsRefused(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "elsewhere")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(base, "remote")); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvPath, filepath.Join(base, "remote", "binding.json"))
	if err := Write(Binding{Root: "/r", Target: "claude:1", NativeSession: "s"}); err == nil {
		t.Fatal("wrote through a symlinked binding directory")
	}
	if _, err := Read(); err == nil {
		t.Fatal("read through a symlinked binding directory")
	}
}
