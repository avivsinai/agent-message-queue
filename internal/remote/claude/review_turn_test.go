package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Codex #868 r2 2026-09-23T08-44-02.855Z_pid79326_2ce7f4df: an older Stop
// in the same batch must not clear a user boundary that came after it.
func TestReviewOlderStopMustNotClearCurrentActivityTurn(t *testing.T) {
	root := t.TempDir()
	dir := claudeSessionsDir(root)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	reg, _ := json.Marshal(map[string]any{"pid": 5151, "sessionId": "sess-abc", "kind": "interactive", "cwd": "/tmp/proj"})
	if err := os.WriteFile(filepath.Join(dir, "5151.json"), reg, 0o600); err != nil {
		t.Fatal(err)
	}
	var notes []ActivityNote
	a := &Attachment{home: root, cfg: config{Pid: 5151}, activitySink: func(n ActivityNote) { notes = append(notes, n) }}
	appendTranscript(t, root,
		`{"type":"user","uuid":"turn-A","timestamp":"2026-09-23T06:00:00Z","message":{"content":"first"}}`,
		`{"type":"assistant","uuid":"answer-A","timestamp":"2026-09-23T06:00:01Z","message":{"content":"done"}}`,
		`{"type":"user","uuid":"turn-B","timestamp":"2026-09-23T06:00:03Z","message":{"content":"second"}}`)
	appendStopMarker(t, root, time.Date(2026, 9, 23, 6, 0, 2, 0, time.UTC).UnixMilli())
	a.pollConfirmations()
	appendTranscript(t, root, `{"type":"assistant","uuid":"answer-B","timestamp":"2026-09-23T06:00:04Z","message":{"content":"second answer"}}`)
	a.pollConfirmations()
	if len(notes) != 4 {
		t.Fatalf("notes=%d", len(notes))
	}
	if got := notes[3].TurnID; got != "turn-B" {
		t.Fatalf("current turn lost after older Stop: got %q want turn-B", got)
	}
}
