//go:build unix

package fsq

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestUnixNoReplaceDLQMovePreservesCollision(t *testing.T) {
	base := t.TempDir()
	if err := EnsureAgentDirs(base, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	const filename = "unix-dlq-collision.md"
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
