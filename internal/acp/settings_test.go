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
// prompt uses it, with no restart. The startup budget here would expire
// before publish; the file's budget lets the turn end on its reply.
func TestPromptReadsTurnTimeoutFromSettingsFile(t *testing.T) {
	s, root := mailboxServer(t)
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
	s.cfg.TurnTimeout = time.Nanosecond
	s.mu.Lock()
	s.ready = true
	s.sessions["s"] = &sessionState{ID: "s"}
	s.mu.Unlock()
	run, _, rpcErr := s.beginPrompt([]byte(`{"sessionId":"s","prompt":[{"type":"text","text":"hi"}]}`))
	if rpcErr != nil || run == nil {
		t.Fatalf("begin prompt: %v", rpcErr)
	}
	replied := false
	res, rpcErr := run(func(any) error {
		if ids := inboxPrompts(t, root); !replied && len(ids) == 1 {
			replied = true
			replyAs(t, root, cockpitThread("session/s"), ids[0], format.KindAnswer, "done")
		}
		return nil
	})
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if got := res.(remotePromptResult); got.StopReason != StopReasonEndTurn {
		t.Fatalf("result=%+v; want the turn to run on the file's turn_timeout", got)
	}
}

// Review of #1023: {"turn_timout":"4s"} was accepted as an empty document, so
// a typo silently reset a working turn timeout to the startup value. An
// unknown key is refused and the last good value stays.
func TestSettingsTypoKeepsTheLastGoodValue(t *testing.T) {
	s, _ := mailboxServer(t)
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
