package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// Codex #868 r2 2026-09-23T08-44-02.855Z_pid79326_2ce7f4df: recoverRun may
// rewind the confirmation cursor, and that rewind must not re-emit activity.
func TestReviewRecoveryDoesNotRepeatRecentActivity(t *testing.T) {
	home := t.TempDir()
	dir := claudeSessionsDir(home)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"pid": 5151, "sessionId": "sess-abc", "kind": "interactive", "cwd": "/tmp/proj"})
	if err := os.WriteFile(filepath.Join(dir, "5151.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// Drive the owning poll method explicitly; cancellation prevents a ticker
	// goroutine from racing the deterministic file append/read sequence.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	att := &Attachment{home: home, cfg: config{Pid: 5151}, ctx: ctx, runs: map[requests.Key]*runRecord{}, recoverFrom: map[requests.Key]recoverScan{}}
	var notes []ActivityNote
	unsub := att.ObserveActivity(func(n ActivityNote) { notes = append(notes, n) })
	defer unsub()
	key := requests.Key{CreatorHost: "owner", TargetID: "claude", RequestID: "12345678-1234-4123-8123-123456789abc"}
	now := time.Now().UTC()
	user, _ := json.Marshal(map[string]any{"type": "user", "uuid": "turn-1", "sessionId": "sess-abc", "timestamp": now.Format(time.RFC3339Nano), "origin": map[string]string{"kind": "peer", "msg_id": frameMsgID(key)}, "message": map[string]any{"content": "recent prompt"}})
	assistant, _ := json.Marshal(map[string]any{"type": "assistant", "uuid": "line-2", "sessionId": "sess-abc", "timestamp": now.Format(time.RFC3339Nano), "message": map[string]any{"content": "recent answer"}})
	appendTranscript(t, home, string(user), string(assistant))
	att.pollConfirmations()
	if len(notes) != 2 {
		t.Fatalf("initial notes=%d", len(notes))
	}
	// A retained endpoint request can become recoverable after the observer
	// reads its delivery; use the real transcript scan and cursor rewind.
	ev := att.recoverRun(key)
	if !ev.Known || att.runs[key] == nil {
		t.Fatalf("not recovered: %+v", ev)
	}
	att.pollConfirmations()
	for _, n := range notes {
		if time.Since(time.UnixMilli(n.Line.TS)) > 30*time.Second {
			t.Fatal("fixture is stale")
		}
	}
	if len(notes) != 2 {
		t.Fatalf("recent activity repeated after actual recoverRun: %d notes; IDs %q %q %q %q", len(notes), notes[0].Line.UUID, notes[1].Line.UUID, notes[2].Line.UUID, notes[3].Line.UUID)
	}
}
