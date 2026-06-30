//go:build windows

package fsq

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReplaceFileWindowsReplacesExistingFile(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "state.json")
	tmpPath := filepath.Join(dir, ".state.json.tmp-test")
	if err := os.WriteFile(finalPath, []byte("old"), 0o600); err != nil {
		t.Fatalf("write final file: %v", err)
	}
	if err := os.WriteFile(tmpPath, []byte("new"), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	if err := replaceFile(tmpPath, finalPath); err != nil {
		t.Fatalf("replaceFile: %v", err)
	}

	data, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("read final file: %v", err)
	}
	if string(data) != "new" {
		t.Fatalf("final data = %q, want new", data)
	}
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatalf("temp path should be gone, stat err=%v", err)
	}
}
