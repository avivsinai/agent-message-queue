package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
)

// Codex #868 review 2026-09-23T07-04-12.081Z_pid46637_0449f9e4: activity
// observes the poller's existing parse. A line without sessionId takes the
// cursor session. The user-line UUID is the turn, and each line UUID stays
// on the note. The core subscription is left in place.
func TestObserveActivityUsesTheExistingParse(t *testing.T) {
	home := t.TempDir()
	const pid = 5151
	regDir := claudeSessionsDir(home)
	if err := os.MkdirAll(regDir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{
		"pid": pid, "sessionId": "sess-abc", "kind": "interactive",
		"cwd": "/tmp/proj",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(regDir, "5151.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	att := &Attachment{home: home, cfg: config{Pid: pid}, ctx: context.Background()}
	att.Subscribe(func(core.NativeEvent) {})
	var notes []ActivityNote
	unsub := att.ObserveActivity(func(n ActivityNote) { notes = append(notes, n) })

	att.mu.Lock()
	coreKept := att.eventSink != nil
	activityKept := att.activitySink != nil
	stopLoop := att.confirmCancel
	att.mu.Unlock()
	if !coreKept || !activityKept || stopLoop == nil {
		t.Fatal("observer did not keep the existing reader and core subscription")
	}
	stopLoop()

	user, err := json.Marshal(map[string]any{
		"type": "user", "uuid": "turn-1",
		"timestamp": "2026-09-23T06:00:00.000000000Z",
		"message":   map[string]any{"role": "user", "content": "hello"},
	})
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := json.Marshal(map[string]any{
		"type": "assistant", "uuid": "line-2",
		"timestamp": "2026-09-23T06:00:01.000000000Z",
		"message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": "ok"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	appendTranscript(t, home, string(user), string(assistant))
	att.pollConfirmations()

	if len(notes) != 2 {
		t.Fatalf("notes = %d, want 2", len(notes))
	}
	for _, n := range notes {
		if n.SessionID != "sess-abc" || n.TurnID != "turn-1" {
			t.Fatalf("note session %q turn %q", n.SessionID, n.TurnID)
		}
	}
	if notes[0].Line.UUID != "turn-1" || notes[1].Line.UUID != "line-2" {
		t.Fatalf("line uuids = %q %q", notes[0].Line.UUID, notes[1].Line.UUID)
	}

	unsub()
	if !att.idleStop() {
		t.Fatal("unsubscribing the observer did not release the reader")
	}
}
