package fsq

import (
	"os"
	"path/filepath"
	"testing"
)

// EnsureAgentDir creates exactly the requested leaf. A writer that owns one
// leaf must not provision the rest of the mailbox as a side effect; an
// implementation that delegates to EnsureAgentDirs fails the inbox assertion.
func TestEnsureAgentDirCreatesOnlyTheRequestedLeaf(t *testing.T) {
	root := t.TempDir()
	if err := EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	identity, err := SnapshotDeliveryRoot(root)
	if err != nil {
		t.Fatalf("SnapshotDeliveryRoot: %v", err)
	}
	delivery, err := OpenDeliveryRoot(root, identity)
	if err != nil {
		t.Fatalf("OpenDeliveryRoot: %v", err)
	}
	defer func() { _ = delivery.Close() }()

	if err := delivery.EnsureAgentDir("codex", MailboxReceipts); err != nil {
		t.Fatalf("EnsureAgentDir: %v", err)
	}
	info, err := os.Stat(filepath.Join(root, "agents", "codex", "receipts"))
	if err != nil || !info.IsDir() {
		t.Fatalf("receipts leaf not created: info=%v err=%v", info, err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("receipts leaf mode = %o, want 0700", info.Mode().Perm())
	}
	for _, other := range []string{"inbox", "outbox", "dlq"} {
		if _, err := os.Lstat(filepath.Join(root, "agents", "codex", other)); !os.IsNotExist(err) {
			t.Fatalf("EnsureAgentDir(receipts) also created %s (err=%v)", other, err)
		}
	}
	if err := delivery.EnsureAgentDir("../escape", MailboxReceipts); err == nil {
		t.Fatal("EnsureAgentDir accepted a path-traversal handle")
	}
}
