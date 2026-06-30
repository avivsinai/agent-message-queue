package fsq

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
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

	data := []byte("cross-project message")
	filename := "xproj.md"
	path, err := DeliverToExistingInbox(root, "codex", filename, data)
	if err != nil {
		t.Fatalf("DeliverToExistingInbox: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected message in new: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("content mismatch: got %q, want %q", got, data)
	}

	// tmp should be empty after delivery
	tmpDir := AgentInboxTmp(root, "codex")
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("ReadDir tmp: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected tmp empty, got %d", len(entries))
	}
}

func TestDeliverToExistingInboxNoDir(t *testing.T) {
	root := t.TempDir()
	// Do NOT create agent dirs — inbox doesn't exist.
	_, err := DeliverToExistingInbox(root, "ghost", "test.md", []byte("nope"))
	if err == nil {
		t.Fatal("expected error for non-existent inbox")
	}
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
	if err := EnsureAgentDirs(root, "ada"); err != nil {
		t.Fatalf("EnsureAgentDirs ada: %v", err)
	}

	cloudNew := AgentInboxNew(root, "claude")
	if err := os.Chmod(cloudNew, 0o555); err != nil {
		t.Fatalf("chmod claude new: %v", err)
	}
	defer func() { _ = os.Chmod(cloudNew, 0o700) }()

	filename := "multi.md"
	_, err := DeliverToInboxes(root, []string{"codex", "claude", "ada"}, filename, []byte("hello"))
	if err == nil {
		t.Fatalf("expected delivery error")
	}
	var partial *PartialDeliveryError
	if !errors.As(err, &partial) {
		t.Fatalf("expected PartialDeliveryError, got %T: %v", err, err)
	}
	if partial.Failed != "claude" {
		t.Fatalf("failed recipient = %q, want claude", partial.Failed)
	}
	if len(partial.Pending) != 1 || partial.Pending[0] != "ada" {
		t.Fatalf("pending recipients = %#v, want [ada]", partial.Pending)
	}

	codexNew := filepath.Join(AgentInboxNew(root, "codex"), filename)
	if got := partial.Delivered["codex"]; got != codexNew {
		t.Fatalf("delivered[codex] = %q, want %q", got, codexNew)
	}
	got, err := os.ReadFile(codexNew)
	if err != nil {
		t.Fatalf("expected committed delivery to remain at %s: %v", codexNew, err)
	}
	if string(got) != "hello" {
		t.Fatalf("committed delivery content = %q, want hello", got)
	}

	cloudTmp := filepath.Join(AgentInboxTmp(root, "claude"), filename)
	if _, err := os.Stat(cloudTmp); !os.IsNotExist(err) {
		t.Fatalf("expected failed tmp to be removed from %s", cloudTmp)
	}

	adaTmp := filepath.Join(AgentInboxTmp(root, "ada"), filename)
	if _, err := os.Stat(adaTmp); !os.IsNotExist(err) {
		t.Fatalf("expected pending tmp to be removed from %s", adaTmp)
	}
	adaNew := filepath.Join(AgentInboxNew(root, "ada"), filename)
	if _, err := os.Stat(adaNew); !os.IsNotExist(err) {
		t.Fatalf("expected pending recipient to have no new delivery at %s", adaNew)
	}
}
