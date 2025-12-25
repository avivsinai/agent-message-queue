package fsq

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFindTmpFilesOlderThan(t *testing.T) {
	root := t.TempDir()
	if err := EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	tmpDir := filepath.Join(root, "agents", "codex", "inbox", "tmp")
	oldPath := filepath.Join(tmpDir, "old.tmp")
	newPath := filepath.Join(tmpDir, "new.tmp")
	if err := os.WriteFile(oldPath, []byte("old"), 0o644); err != nil {
		t.Fatalf("write old: %v", err)
	}
	if err := os.WriteFile(newPath, []byte("new"), 0o644); err != nil {
		t.Fatalf("write new: %v", err)
	}

	oldTime := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes old: %v", err)
	}

	cutoff := time.Now().Add(-36 * time.Hour)
	matches, err := FindTmpFilesOlderThan(root, cutoff)
	if err != nil {
		t.Fatalf("FindTmpFilesOlderThan: %v", err)
	}
	if len(matches) != 1 || matches[0] != oldPath {
		t.Fatalf("unexpected matches: %v", matches)
	}
}
