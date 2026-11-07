package claude

import (
	"os"
	"path/filepath"
	"testing"
)

// Review of the hook binary resolver: started as a Homebrew bin symlink, it
// returned the versioned Cellar path that brew cleanup deletes.
func TestHookBinaryKeepsTheSymlinkBesideTheProgram(t *testing.T) {
	base := t.TempDir()
	t.Setenv("TMPDIR", filepath.Join(base, "tmp"))
	t.Setenv("PATH", filepath.Join(base, "empty"))
	cellar, bin := filepath.Join(base, "Cellar"), filepath.Join(base, "bin")
	for _, dir := range []string{cellar, bin} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"amq-acp", "amq-remote"} {
		if err := os.WriteFile(filepath.Join(cellar, name), nil, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(cellar, name), filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	saved := executable
	t.Cleanup(func() { executable = saved })
	executable = func() (string, error) { return filepath.Join(bin, "amq-acp"), nil }
	if got, err := HookBinary("amq-remote"); err != nil || got != filepath.Join(bin, "amq-remote") {
		t.Fatalf("HookBinary = %q, %v; want %s", got, err, filepath.Join(bin, "amq-remote"))
	}
}

// Review of the hook binary resolver: a sibling beside the started path
// that points at an older version won over the one beside the running
// binary.
func TestSiblingBinaryIgnoresAStartedSiblingOfAnotherVersion(t *testing.T) {
	base := t.TempDir()
	bin, oldDir, newDir := filepath.Join(base, "bin"), filepath.Join(base, "Cellar", "1"), filepath.Join(base, "Cellar", "2")
	for _, dir := range []string{bin, oldDir, newDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{filepath.Join(newDir, "amq-acp"), filepath.Join(newDir, "amq-remote"), filepath.Join(oldDir, "amq-remote")} {
		if err := os.WriteFile(path, nil, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(newDir, "amq-acp"), filepath.Join(bin, "amq-acp")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(oldDir, "amq-remote"), filepath.Join(bin, "amq-remote")); err != nil {
		t.Fatal(err)
	}
	saved := executable
	t.Cleanup(func() { executable = saved })
	executable = func() (string, error) { return filepath.Join(bin, "amq-acp"), nil }
	want, _ := filepath.EvalSymlinks(filepath.Join(newDir, "amq-remote"))
	if got, err := SiblingBinary("amq-remote"); err != nil || got != want {
		t.Fatalf("SiblingBinary = %q, %v; want %s", got, err, want)
	}
}
