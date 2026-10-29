package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// Regression: #868, activity
// observes the poller's existing parse. A line without sessionId takes the
// cursor session. The user-line UUID is the turn, and each line UUID stays
// on the note. The core subscription is left in place.
func TestObserveActivityUsesTheExistingParse(t *testing.T) {
	home := t.TempDir()
	const pid = 5151
	regDir := registryDir(defaultConfigDir(home))
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

	att := &Attachment{home: home, configDir: defaultConfigDir(home), cfg: config{Pid: pid}, ctx: context.Background()}
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

// Regression: #868, an older Stop
// in the same batch must not clear a user boundary that came after it.
func TestOlderStopMustNotClearCurrentActivityTurn(t *testing.T) {
	root := t.TempDir()
	dir := registryDir(defaultConfigDir(root))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	reg, _ := json.Marshal(map[string]any{"pid": 5151, "sessionId": "sess-abc", "kind": "interactive", "cwd": "/tmp/proj"})
	if err := os.WriteFile(filepath.Join(dir, "5151.json"), reg, 0o600); err != nil {
		t.Fatal(err)
	}
	var notes []ActivityNote
	a := &Attachment{home: root, configDir: defaultConfigDir(root), cfg: config{Pid: 5151}, activitySink: func(n ActivityNote) { notes = append(notes, n) }}
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

// Regression: #868, recoverRun may
// rewind the confirmation cursor, and that rewind must not re-emit activity.
func TestRecoveryDoesNotRepeatRecentActivity(t *testing.T) {
	home := t.TempDir()
	dir := registryDir(defaultConfigDir(home))
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
	att := &Attachment{home: home, configDir: defaultConfigDir(home), cfg: config{Pid: 5151}, ctx: ctx, runs: map[requests.Key]*runRecord{}, recoverFrom: map[requests.Key]recoverScan{}}
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

// Regression: #868, truncating the
// transcript is a new file generation. The activity watermark must restart
// so the replacement line is emitted.
func TestTruncatedTranscriptEmitsNewActivity(t *testing.T) {
	home := t.TempDir()
	dir := registryDir(defaultConfigDir(home))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"pid": 5151, "sessionId": "sess-abc", "kind": "interactive", "cwd": "/tmp/proj"})
	if err := os.WriteFile(filepath.Join(dir, "5151.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	att := &Attachment{home: home, configDir: defaultConfigDir(home), cfg: config{Pid: 5151}, ctx: ctx}
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
	path := transcriptPath(defaultConfigDir(home), "/tmp/proj", "sess-abc")
	if err := os.WriteFile(path, []byte(makeLine("after", "new prompt")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	att.pollConfirmations()
	att.pollConfirmations()
	if len(notes) != 2 || notes[1].Line.UUID != "after" {
		t.Fatalf("truncated file's new activity suppressed: notes=%d cur=%d watermark=%d", len(notes), att.cur.off, att.activityNext)
	}
}
