//go:build windows

package fsq

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsNoReplacePublicationPreservesCollisionAndIdempotence(t *testing.T) {
	base := t.TempDir()
	if err := EnsureAgentDirs(base, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	const filename = "windows-collision.md"
	retained := []byte("retained unread message")
	conflict := []byte("different colliding message")
	newPath := filepath.Join(AgentInboxNew(base, "alice"), filename)
	if err := os.WriteFile(newPath, retained, 0o600); err != nil {
		t.Fatal(err)
	}

	root := openDeliveryRootForTest(t, base)
	if _, err := DeliverToInbox(root, "alice", filename, conflict); !errors.Is(err, os.ErrExist) {
		t.Fatalf("conflicting publication error = %T %v, want os.ErrExist", err, err)
	}
	got, err := os.ReadFile(newPath)
	if err != nil || !bytes.Equal(got, retained) {
		t.Fatalf("new bytes = %q, %v; collision must not replace unread mail", got, err)
	}

	if _, err := DeliverToInbox(root, "alice", filename, retained); err != nil {
		t.Fatalf("identical-byte publication = %v, want idempotent success", err)
	}
	got, err = os.ReadFile(newPath)
	if err != nil || !bytes.Equal(got, retained) {
		t.Fatalf("idempotent new bytes = %q, %v", got, err)
	}

	entries, err := os.ReadDir(AgentInboxTmp(base, "alice"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("tmp entries = %d, want one retained conflicting attempt", len(entries))
	}
	leftover, err := os.ReadFile(filepath.Join(AgentInboxTmp(base, "alice"), entries[0].Name()))
	if err != nil || !bytes.Equal(leftover, conflict) {
		t.Fatalf("retained tmp bytes = %q, %v", leftover, err)
	}
}

func TestWindowsNoReplaceDLQMovePreservesCollision(t *testing.T) {
	base := t.TempDir()
	if err := EnsureAgentDirs(base, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	const filename = "windows-dlq-collision.md"
	newPath := filepath.Join(AgentDLQNew(base, "alice"), filename)
	curPath := filepath.Join(AgentDLQCur(base, "alice"), filename)
	if err := os.WriteFile(newPath, []byte("new envelope"), 0o600); err != nil {
		t.Fatal(err)
	}

	root := openDeliveryRootForTest(t, base)
	oldHook := beforeMoveDLQNewToCurRenameForTest
	beforeMoveDLQNewToCurRenameForTest = func(_ *DeliveryRoot, _, destination string) {
		if err := os.WriteFile(filepath.Join(base, destination), []byte("retained envelope"), 0o600); err != nil {
			t.Errorf("create racing DLQ cur: %v", err)
		}
	}
	t.Cleanup(func() { beforeMoveDLQNewToCurRenameForTest = oldHook })
	err := MoveDLQNewToCur(root, "alice", filename)
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("DLQ no-replace move error = %T %v, want os.ErrExist", err, err)
	}
	got, readErr := os.ReadFile(curPath)
	if readErr != nil || string(got) != "retained envelope" {
		t.Fatalf("cur bytes = %q, %v; no-replace move must preserve destination", got, readErr)
	}
	got, readErr = os.ReadFile(newPath)
	if readErr != nil || string(got) != "new envelope" {
		t.Fatalf("new bytes = %q, %v; failed move must preserve source", got, readErr)
	}
}
