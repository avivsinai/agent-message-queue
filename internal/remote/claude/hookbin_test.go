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
