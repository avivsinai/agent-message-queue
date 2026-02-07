package fsq

import (
	"os"
	"path/filepath"
	"runtime"
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
	dlqTmpDir := filepath.Join(root, "agents", "codex", "dlq", "tmp")
	outboxDir := filepath.Join(root, "agents", "codex", "outbox", "sent")
	if err := os.MkdirAll(dlqTmpDir, 0o700); err != nil {
		t.Fatalf("mkdir dlq tmp: %v", err)
	}
	if err := os.MkdirAll(outboxDir, 0o700); err != nil {
		t.Fatalf("mkdir outbox: %v", err)
	}

	oldPath := filepath.Join(tmpDir, "old.tmp")
	dlqOldPath := filepath.Join(dlqTmpDir, "dlq-old.tmp")
	dotOldPath := filepath.Join(outboxDir, ".outbox.tmp-123")
	newPath := filepath.Join(tmpDir, "new.tmp")
	if err := os.WriteFile(oldPath, []byte("old"), 0o644); err != nil {
		t.Fatalf("write old: %v", err)
	}
	if err := os.WriteFile(dlqOldPath, []byte("old dlq"), 0o644); err != nil {
		t.Fatalf("write dlq old: %v", err)
	}
	if err := os.WriteFile(dotOldPath, []byte("old dot"), 0o644); err != nil {
		t.Fatalf("write dot tmp: %v", err)
	}
	if err := os.WriteFile(newPath, []byte("new"), 0o644); err != nil {
		t.Fatalf("write new: %v", err)
	}

	oldTime := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes old: %v", err)
	}
	if err := os.Chtimes(dlqOldPath, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes dlq old: %v", err)
	}
	if err := os.Chtimes(dotOldPath, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes dot old: %v", err)
	}

	cutoff := time.Now().Add(-36 * time.Hour)
	matches, err := FindTmpFilesOlderThan(root, cutoff)
	if err != nil {
		t.Fatalf("FindTmpFilesOlderThan: %v", err)
	}
	expected := map[string]struct{}{
		oldPath:    {},
		dlqOldPath: {},
		dotOldPath: {},
	}
	if len(matches) != len(expected) {
		t.Fatalf("unexpected matches: %v", matches)
	}
	for _, match := range matches {
		if _, ok := expected[match]; !ok {
			t.Fatalf("unexpected match: %s", match)
		}
	}
}

func TestFindTmpFilesOlderThan_WalkDirError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod permissions are unreliable on Windows")
	}
	root := t.TempDir()
	if err := EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Make the agent directory unreadable to cause a WalkDir error
	agentDir := filepath.Join(root, "agents", "codex")
	// Create a subdirectory and make it unreadable
	subDir := filepath.Join(agentDir, "inbox")
	if err := os.Chmod(subDir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer func() { _ = os.Chmod(subDir, 0o700) }() // restore for cleanup

	cutoff := time.Now().Add(-1 * time.Hour)
	_, err := FindTmpFilesOlderThan(root, cutoff)
	if err == nil {
		t.Fatal("expected error from WalkDir on unreadable directory")
	}
}
