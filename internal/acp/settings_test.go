package acp

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/amqhome"
	"github.com/avivsinai/agent-message-queue/internal/format"
)

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

// Issue 1023 for an owner turn of the machine agent: the next owner message
// runs on the file's turn_timeout. The startup budget here would expire
// before publish; the file's budget lets the turn end on its reply.
func TestOwnerTurnReadsTurnTimeoutFromSettingsFile(t *testing.T) {
	f, b := mailboxRoute(t)
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
	f.s.cfg.TurnTimeout = time.Nanosecond
	f.machine(t)
	in := f.ownerSays(t, machineChanA, "hi")
	done := f.s.Owner(in)
	id, prompt := promptOf(t, b)
	reply := replyAs(t, b.Root, prompt.Header.Thread, id, format.KindAnswer, "done")
	waitClosed(t, done)
	if out, _ := f.s.mailboxAnswered(in.Event.ID.Hex()); out.ReplyID != reply || f.posts(machineChanA, "done") != 1 {
		t.Fatalf("outcome=%+v posts=%q; want the turn to run on the file's turn_timeout and end on its reply", out, f.agentTexts(machineChanA))
	}
}

// Regression: #1023, {"turn_timout":"4s"} was accepted as an empty document, so
// a typo silently reset a working turn timeout to the startup value. An
// unknown key is refused and the last good value stays.
func TestSettingsTypoKeepsTheLastGoodValue(t *testing.T) {
	s := standalone(t).s
	t.Setenv("HOME", canonicalTempDir(t))
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	dir, err := amqhome.EnsureDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, settingsFile)
	for _, body := range []string{`{"turn_timeout":"30m"}`, `{"turn_timout":"4s"}`} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := s.loadTurnTiming().timeout; got != 30*time.Minute {
			t.Fatalf("after %s turn timeout=%v; want 30m", body, got)
		}
	}
}
