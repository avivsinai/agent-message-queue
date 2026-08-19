//go:build !windows

package fsq

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReplaceFileMissingTmpLeavesDestIntact(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "state.json")
	if err := os.WriteFile(finalPath, []byte("keep-me"), 0o600); err != nil {
		t.Fatalf("write dest: %v", err)
	}
	missingTmp := filepath.Join(dir, ".state.json.tmp-missing")

	err := replaceFile(missingTmp, finalPath)
	if err == nil {
		t.Fatal("replaceFile(missing tmp) error = nil, want error")
	}

	got, readErr := os.ReadFile(finalPath)
	if readErr != nil {
		t.Fatalf("dest unreadable after failed replace: %v", readErr)
	}
	if string(got) != "keep-me" {
		t.Fatalf("dest bytes = %q, want keep-me", got)
	}
}

func TestReplaceFileMissingTmpLeavesEmptyDestDir(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "emptydir")
	if err := os.Mkdir(finalPath, 0o700); err != nil {
		t.Fatalf("mkdir dest: %v", err)
	}
	missingTmp := filepath.Join(dir, ".emptydir.tmp-missing")

	err := replaceFile(missingTmp, finalPath)
	if err == nil {
		t.Fatal("replaceFile(missing tmp) error = nil, want error")
	}

	info, statErr := os.Stat(finalPath)
	if statErr != nil {
		t.Fatalf("empty dest dir removed: %v", statErr)
	}
	if !info.IsDir() {
		t.Fatalf("dest mode = %v, want directory", info.Mode())
	}
}
