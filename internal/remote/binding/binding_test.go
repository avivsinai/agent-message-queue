package binding

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Codex #885 P2 #3: an off for session A must not remove a binding that a
// concurrent attach already moved to session B.
func TestRemoveLeavesAnotherSessionsBinding(t *testing.T) {
	t.Setenv(EnvPath, filepath.Join(canonicalTempDir(t), "remote", "binding.json"))
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
	base := canonicalTempDir(t)
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

// canonicalTempDir is t.TempDir with symlinks resolved: the override refuses
// a symlinked path, and macOS temp dirs live under the /var symlink.
func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// Codex #885 r2 P2: the override followed a symlink above its own directory.
func TestOverrideWithSymlinkedAncestorIsRefused(t *testing.T) {
	base := canonicalTempDir(t)
	if err := os.MkdirAll(filepath.Join(base, "real", "remote"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvPath, filepath.Join(base, "link", "remote", "binding.json"))
	if err := Write(Binding{Root: "/r", Target: "claude:1", NativeSession: "s"}); err == nil {
		t.Fatal("wrote through a symlinked ancestor")
	}
}

// Bead agent-message-queue-94w (review F3): two sessions whose default name
// collides must not silently replace each other; the same session may attach
// again.
func TestWriteNamedNewRefusesAnotherSessionsName(t *testing.T) {
	t.Setenv(EnvPath, filepath.Join(canonicalTempDir(t), "remote", "binding.json"))
	a := Binding{Carrier: CarrierMailbox, Root: "/p/.agent-mail/a", Handle: "claude", Name: "claude-p"}
	b := Binding{Carrier: CarrierMailbox, Root: "/p/.agent-mail/b", Handle: "claude", Name: "claude-p"}
	if err := WriteNamedNew(a); err != nil {
		t.Fatal(err)
	}
	if err := WriteNamedNew(a); err != nil {
		t.Fatalf("re-attaching the same session: %v", err)
	}
	var taken *NameTakenError
	if err := WriteNamedNew(b); !errors.As(err, &taken) {
		t.Fatalf("second session with the same name: err=%v; want NameTakenError", err)
	}
	if got, err := ReadNamed("claude-p"); err != nil || !got.Same(a) {
		t.Fatalf("binding = %+v %v; want session a kept", got, err)
	}
}
