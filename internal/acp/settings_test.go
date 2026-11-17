package acp

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/amqhome"
)

// settingsFile is the owner's settings file below the per-user home.
const settingsFile = "acp.settings"

// Issue 1023: the owner raises turn_timeout in acp.settings and the next
// ACP prompt uses it, with no restart. The startup budget here ends the
// turn at its first wait; the file's budget lets it end on the reply that
// arrives at the first heartbeat.
func TestPromptReadsTurnTimeoutFromSettingsFile(t *testing.T) {
	cfg := testConfig(t)
	home := canonicalTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir, err := amqhome.EnsureDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, settingsFile), []byte(`{"turn_timeout":"2s"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.TurnTimeout = time.Nanosecond
	s := NewServer(cfg, "test")
	thread := cockpitThread("session/s")
	s.mu.Lock()
	s.ready = true
	s.sessions["s"] = &sessionState{ID: "s", Thread: thread}
	s.mu.Unlock()
	run, _, rpcErr := s.beginPrompt([]byte(`{"sessionId":"s","prompt":[{"type":"text","text":"hi"}]}`))
	if rpcErr != nil || run == nil {
		t.Fatalf("begin prompt: %v", rpcErr)
	}
	updates := 0
	res, rpcErr := run(func(any) error {
		// The first update says the turn waits; the second is a heartbeat.
		if updates++; updates == 2 {
			deliverReply(t, cfg, thread, "done")
		}
		return nil
	})
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if got := res.(promptResult); got.StopReason != StopReasonEndTurn || got.Meta.AMQ.Reply != "done" {
		t.Fatalf("result=%+v; want the turn to run on the file's turn_timeout and end on its reply", got)
	}
}
