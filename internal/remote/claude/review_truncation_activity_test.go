package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Codex #868 r3 2026-09-23T09-22-12.363Z_pid77460_2fd2e2c6: truncating the
// transcript is a new file generation. The activity watermark must restart
// so the replacement line is emitted.
func TestReviewTruncatedTranscriptEmitsNewActivity(t *testing.T) {
	home := t.TempDir()
	dir := claudeSessionsDir(home)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"pid": 5151, "sessionId": "sess-abc", "kind": "interactive", "cwd": "/tmp/proj"})
	if err := os.WriteFile(filepath.Join(dir, "5151.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	att := &Attachment{home: home, cfg: config{Pid: 5151}, ctx: ctx}
	var notes []ActivityNote
	unsub := att.ObserveActivity(func(n ActivityNote) { notes = append(notes, n) })
	defer unsub()
	makeLine := func(id, text string) string {
		raw, _ := json.Marshal(map[string]any{"type": "user", "uuid": id, "sessionId": "sess-abc", "timestamp": time.Now().UTC().Format(time.RFC3339Nano), "message": map[string]any{"content": text}})
		return string(raw)
	}
	appendTranscript(t, home, makeLine("before", strings.Repeat("x", 4096)))
	att.pollConfirmations()
	if len(notes) != 1 {
		t.Fatalf("initial notes=%d", len(notes))
	}
	// Exercise the reader's supported truncation branch, not a manual cursor reset.
	path := transcriptPath(home, "/tmp/proj", "sess-abc")
	if err := os.WriteFile(path, []byte(makeLine("after", "new prompt")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	att.pollConfirmations()
	att.pollConfirmations()
	if len(notes) != 2 || notes[1].Line.UUID != "after" {
		t.Fatalf("truncated file's new activity suppressed: notes=%d cur=%d watermark=%d", len(notes), att.cur.off, att.activityNext)
	}
}
