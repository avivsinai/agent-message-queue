package fsq

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDeliverToInbox(t *testing.T) {
	root := t.TempDir()
	if err := EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	data := []byte("hello")
	filename := "test.md"
	path, err := DeliverToInbox(root, "codex", filename, data)
	if err != nil {
		t.Fatalf("DeliverToInbox: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected message in new: %v", err)
	}
	tmpDir := filepath.Join(root, "agents", "codex", "inbox", "tmp")
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("ReadDir tmp: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected tmp empty, got %d", len(entries))
	}
}

func TestMoveNewToCur(t *testing.T) {
	root := t.TempDir()
	if err := EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	data := []byte("hello")
	filename := "move.md"
	if _, err := DeliverToInbox(root, "codex", filename, data); err != nil {
		t.Fatalf("DeliverToInbox: %v", err)
	}
	if err := MoveNewToCur(root, "codex", filename); err != nil {
		t.Fatalf("MoveNewToCur: %v", err)
	}
	newPath := filepath.Join(root, "agents", "codex", "inbox", "new", filename)
	curPath := filepath.Join(root, "agents", "codex", "inbox", "cur", filename)
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Fatalf("expected new missing")
	}
	if _, err := os.Stat(curPath); err != nil {
		t.Fatalf("expected cur present: %v", err)
	}
}
