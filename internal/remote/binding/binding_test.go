package binding

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
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

// Pro review of 94w: a trailing slash on the same root was refused as a
// collision when the same session attached again.
func TestSameIgnoresRootSpelling(t *testing.T) {
	a := Binding{Carrier: CarrierMailbox, Root: "/p/.agent-mail/a", Handle: "claude"}
	if !a.Same(Binding{Carrier: CarrierMailbox, Root: "/p/.agent-mail/a/", Handle: "claude"}) {
		t.Fatal("trailing-slash root not Same as the clean root")
	}
}

// review r1: EnsureDir now refuses a group-writable ~/.amq. With umask 0002
// the amq-remote skill's mkdir -p makes ~/.amq 0775, and attach refused it.
func TestWriteAcceptsGroupWritableAMQHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix mode bits; HOME does not isolate the Windows home")
	}
	home := canonicalTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv(EnvPath, "")
	amq := filepath.Join(home, ".amq")
	if err := os.Mkdir(amq, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(amq, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := Write(Binding{Root: "/r", Target: "claude:1", NativeSession: "s"}); err != nil {
		t.Fatal(err)
	}
}

// Regression (merged-code e2e finding 1): an AMQ_REMOTE_BINDING whose
// directory's parent did not exist yet was refused as "must have no symlink
// on its path", although no symlink was involved. The missing directories are
// now created (0700) below a canonical ancestor.
func TestBindingOverrideCreatesMissingDirectories(t *testing.T) {
	base := canonicalTempDir(t)
	t.Setenv(EnvPath, filepath.Join(base, "home", ".amq", "remote", "binding.json"))
	b := Binding{Root: "/r", Target: "claude:1", NativeSession: "s"}
	if err := Write(b); err != nil {
		t.Fatalf("Write = %v, want the missing directories created", err)
	}
	if got, err := Read(); err != nil || !got.Same(b) {
		t.Fatalf("binding = %+v %v", got, err)
	}
}

// Review B1 of #1041: a symlink swapped in for a missing level after the
// ancestor check must not redirect the binding (MkdirAll followed it): Write
// refuses and creates nothing under the link's target. An ancestor others can
// write is refused too.
func TestBindingOverrideRefusesASymlinkSwappedInAfterTheCheck(t *testing.T) {
	base, elsewhere := canonicalTempDir(t), canonicalTempDir(t)
	t.Setenv(EnvPath, filepath.Join(base, "home", ".amq", "remote", "binding.json"))
	overrideCheckedHook = func() {
		if err := os.Symlink(elsewhere, filepath.Join(base, "home")); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { overrideCheckedHook = nil })
	b := Binding{Root: "/r", Target: "claude:1", NativeSession: "s"}
	if err := Write(b); err == nil {
		t.Fatal("Write followed a symlink swapped in after the check")
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Fatalf("created %v under the link's target", entries)
	}
	overrideCheckedHook = nil
	shared := filepath.Join(base, "shared")
	if err := os.Mkdir(shared, 0o700); err != nil || os.Chmod(shared, 0o777) != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvPath, filepath.Join(shared, "remote", "binding.json"))
	if err := Write(b); err == nil {
		t.Fatal("Write created the binding below a world-writable directory")
	}
}
