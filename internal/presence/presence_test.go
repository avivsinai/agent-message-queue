package presence

import (
	"testing"
	"time"
)

func TestPresenceWriteRead(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2025, 12, 24, 15, 2, 33, 0, time.UTC)
	p := New("codex", "busy", "reviewing", now)
	if err := Write(root, p); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Read(root, "codex")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Handle != "codex" || got.Status != "busy" || got.Note != "reviewing" {
		t.Fatalf("unexpected presence: %+v", got)
	}
	if got.LastSeen == "" {
		t.Fatalf("expected LastSeen to be set")
	}
}
