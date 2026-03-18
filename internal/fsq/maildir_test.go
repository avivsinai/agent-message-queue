package fsq

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

func TestDeliverToExistingInbox(t *testing.T) {
	root := t.TempDir()
	if err := EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	t.Run("success when inbox dirs exist", func(t *testing.T) {
		filename := "existing-ok.md"
		data := []byte("hello federated")
		path, err := DeliverToExistingInbox(root, "codex", filename, data)
		if err != nil {
			t.Fatalf("expected success, got: %v", err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected message in new: %v", err)
		}
		// tmp must be empty after delivery
		tmpDir := AgentInboxTmp(root, "codex")
		entries, err := os.ReadDir(tmpDir)
		if err != nil {
			t.Fatalf("ReadDir tmp: %v", err)
		}
		if len(entries) != 0 {
			t.Fatalf("expected tmp empty after delivery, got %d entries", len(entries))
		}
	})

	t.Run("does not create directories", func(t *testing.T) {
		missingRoot := t.TempDir()
		// Note: EnsureAgentDirs is NOT called for "ghost" agent.
		_, err := DeliverToExistingInbox(missingRoot, "ghost", "ghost.md", []byte("nope"))
		if err == nil {
			t.Fatal("expected error when inbox dirs are missing")
		}
		// Confirm no directories were created
		if _, statErr := os.Stat(AgentInboxTmp(missingRoot, "ghost")); statErr == nil {
			t.Fatal("DeliverToExistingInbox must not create tmp dir")
		}
		if _, statErr := os.Stat(AgentInboxNew(missingRoot, "ghost")); statErr == nil {
			t.Fatal("DeliverToExistingInbox must not create new dir")
		}
	})

	t.Run("fails when tmp dir missing", func(t *testing.T) {
		partialRoot := t.TempDir()
		if err := EnsureRootDirs(partialRoot); err != nil {
			t.Fatalf("EnsureRootDirs: %v", err)
		}
		// Create only the new dir, not tmp.
		newDir := AgentInboxNew(partialRoot, "partial")
		if err := os.MkdirAll(newDir, 0o700); err != nil {
			t.Fatalf("MkdirAll new: %v", err)
		}
		_, err := DeliverToExistingInbox(partialRoot, "partial", "test.md", []byte("data"))
		if err == nil {
			t.Fatal("expected error when tmp dir missing")
		}
	})

	t.Run("fails when new dir missing", func(t *testing.T) {
		partialRoot := t.TempDir()
		if err := EnsureRootDirs(partialRoot); err != nil {
			t.Fatalf("EnsureRootDirs: %v", err)
		}
		// Create only the tmp dir, not new.
		tmpDir := AgentInboxTmp(partialRoot, "partial2")
		if err := os.MkdirAll(tmpDir, 0o700); err != nil {
			t.Fatalf("MkdirAll tmp: %v", err)
		}
		_, err := DeliverToExistingInbox(partialRoot, "partial2", "test.md", []byte("data"))
		if err == nil {
			t.Fatal("expected error when new dir missing")
		}
	})

	t.Run("rejects invalid agent handle", func(t *testing.T) {
		_, err := DeliverToExistingInbox(root, "../escape", "test.md", []byte("data"))
		if err == nil {
			t.Fatal("expected error for path-traversal agent handle")
		}
		if !strings.Contains(err.Error(), "invalid agent handle") {
			t.Fatalf("unexpected error message: %v", err)
		}
	})

	t.Run("rejects path traversal in filename", func(t *testing.T) {
		_, err := DeliverToExistingInbox(root, "codex", "../escape.md", []byte("data"))
		if err == nil {
			t.Fatal("expected error for path-traversal filename")
		}
	})

	t.Run("rejects empty filename", func(t *testing.T) {
		_, err := DeliverToExistingInbox(root, "codex", "", []byte("data"))
		if err == nil {
			t.Fatal("expected error for empty filename")
		}
	})

	t.Run("atomic rename: message appears in new not tmp", func(t *testing.T) {
		filename := "atomic-check.md"
		data := []byte("atomic data")
		path, err := DeliverToExistingInbox(root, "codex", filename, data)
		if err != nil {
			t.Fatalf("DeliverToExistingInbox: %v", err)
		}
		// Message must be in new/
		expectedNew := filepath.Join(AgentInboxNew(root, "codex"), filename)
		if path != expectedNew {
			t.Fatalf("expected path %s, got %s", expectedNew, path)
		}
		// Message must NOT be in tmp/
		tmpPath := filepath.Join(AgentInboxTmp(root, "codex"), filename)
		if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
			t.Fatal("message should not remain in tmp after delivery")
		}
	})
}

func TestDeliverToInboxesRollback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod permissions are unreliable on Windows")
	}
	root := t.TempDir()
	if err := EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs codex: %v", err)
	}
	if err := EnsureAgentDirs(root, "claude"); err != nil {
		t.Fatalf("EnsureAgentDirs claude: %v", err)
	}

	cloudNew := AgentInboxNew(root, "claude")
	if err := os.Chmod(cloudNew, 0o555); err != nil {
		t.Fatalf("chmod claude new: %v", err)
	}

	filename := "multi.md"
	if _, err := DeliverToInboxes(root, []string{"codex", "claude"}, filename, []byte("hello")); err == nil {
		t.Fatalf("expected delivery error")
	}

	codexNew := filepath.Join(AgentInboxNew(root, "codex"), filename)
	if _, err := os.Stat(codexNew); !os.IsNotExist(err) {
		t.Fatalf("expected rollback to remove %s", codexNew)
	}

	cloudTmp := filepath.Join(AgentInboxTmp(root, "claude"), filename)
	if _, err := os.Stat(cloudTmp); !os.IsNotExist(err) {
		t.Fatalf("expected rollback to remove %s", cloudTmp)
	}
}
