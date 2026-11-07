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
