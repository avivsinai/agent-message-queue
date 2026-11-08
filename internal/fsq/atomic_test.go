package fsq

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteStateFileReplacesExistingFile(t *testing.T) {
	dir := t.TempDir()

	finalPath, err := WriteStateFile(dir, "state.json", []byte("old"), 0o600)
	if err != nil {
		t.Fatalf("initial WriteStateFile: %v", err)
	}
	if _, err := WriteStateFile(dir, "state.json", []byte("new"), 0o600); err != nil {
		t.Fatalf("replacement WriteStateFile: %v", err)
	}

	data, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("read final file: %v", err)
	}
	if string(data) != "new" {
		t.Fatalf("final data = %q, want new", data)
	}
	tmpMatches, err := filepath.Glob(filepath.Join(dir, ".state.json.tmp-*"))
	if err != nil {
		t.Fatalf("glob temp files: %v", err)
	}
	if len(tmpMatches) != 0 {
		t.Fatalf("temporary files remain: %v", tmpMatches)
	}
}

// Review of the durable-write contract: a state file in a new directory
// synced only that directory, so the new directory's own entry could be lost
// on power loss while the write reported success.
func TestWriteStateFileSyncsTheParentOfEachDirectoryItCreates(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	synced := map[string]bool{}
	restore := SetPackageSyncDirFaultForTest(func(dir string) error {
		synced[dir] = true
		return nil
	})
	defer restore()

	if _, err := WriteStateFile(root, filepath.Join("a", "b", "state.json"), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteStateFile: %v", err)
	}
	for _, dir := range []string{root, filepath.Join(root, "a")} {
		if !synced[dir] {
			t.Errorf("%s owns a new directory's entry and was not synced; synced: %v", dir, synced)
		}
	}
}
