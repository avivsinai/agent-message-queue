//go:build darwin || linux

package fsq

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMoveCurToDLQRejectsEscapingTmpSymlink(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	filename := "escape_tmp.md"
	source := filepath.Join(AgentInboxCur(root, "alice"), filename)
	if err := os.WriteFile(source, []byte("contained"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}

	outside := t.TempDir()
	tmpDir := AgentDLQTmp(root, "alice")
	if err := os.RemoveAll(tmpDir); err != nil {
		t.Fatalf("remove dlq tmp: %v", err)
	}
	if err := os.Symlink(outside, tmpDir); err != nil {
		t.Fatalf("symlink dlq tmp: %v", err)
	}

	if _, err := MoveCurToDLQ(openDeliveryRootForTest(t, root), "alice", filename, "escape_tmp", "parse_error", "test"); err == nil {
		t.Fatal("MoveCurToDLQ succeeded through an escaping dlq/tmp symlink")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatalf("read outside: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("escaping dlq/tmp received %d files", len(entries))
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("source should remain after refused DLQ delivery: %v", err)
	}
}
