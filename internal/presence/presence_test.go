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

func TestTouchCreatesPresence(t *testing.T) {
	root := t.TempDir()
	if err := Touch(root, "alice"); err != nil {
		t.Fatalf("Touch (new): %v", err)
	}
	got, err := Read(root, "alice")
	if err != nil {
		t.Fatalf("Read after touch: %v", err)
	}
	if got.Handle != "alice" || got.Status != "active" {
		t.Fatalf("unexpected presence: %+v", got)
	}
}

func TestTouchPreservesExisting(t *testing.T) {
	root := t.TempDir()
	old := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	p := New("bob", "busy", "deep work", old)
	if err := Write(root, p); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := Touch(root, "bob"); err != nil {
		t.Fatalf("Touch (existing): %v", err)
	}
	got, err := Read(root, "bob")
	if err != nil {
		t.Fatalf("Read after touch: %v", err)
	}
	if got.Status != "busy" || got.Note != "deep work" {
		t.Fatalf("Touch should preserve status/note, got: %+v", got)
	}
	ts, err := time.Parse(time.RFC3339Nano, got.LastSeen)
	if err != nil {
		t.Fatalf("parse LastSeen: %v", err)
	}
	if time.Since(ts) > 5*time.Second {
		t.Fatalf("LastSeen not updated: %s", got.LastSeen)
	}
}

func TestNoteAtExistsIndependentlyOfWakeTouches(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	if err := Write(root, New("worker", "busy", "waiting", old)); err != nil {
		t.Fatal(err)
	}
	if err := Touch(root, "worker"); err != nil {
		t.Fatal(err)
	}
	oldPresence, _ := Read(root, "worker")
	if oldPresence.NoteAt == "" || oldPresence.NoteAt == oldPresence.LastSeen {
		t.Fatalf("note timestamp=%q last_seen=%q", oldPresence.NoteAt, oldPresence.LastSeen)
	}
	if _, err := SetStatus(root, "worker", "off", "waiting"); err != nil {
		t.Fatal(err)
	}
	same, _ := Read(root, "worker")
	if same.NoteAt != oldPresence.NoteAt {
		t.Fatalf("same note refreshed: %q -> %q", oldPresence.NoteAt, same.NoteAt)
	}
	if _, err := SetStatus(root, "worker", "off", "done"); err != nil {
		t.Fatal(err)
	}
	next, _ := Read(root, "worker")
	if next.NoteAt == same.NoteAt {
		t.Fatal("changed note did not get its new timestamp")
	}
}
