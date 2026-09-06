//go:build darwin || linux

package cli

import (
	"os"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func newGuardedWakeAttentionConfig(
	root string,
	inputWrites *int,
	attention *[]string,
) *wakeConfig {
	return &wakeConfig{
		root:          root,
		me:            "codex",
		session:       "session1",
		injectMode:    wakeInjectModeRaw,
		previewLen:    80,
		controlStop:   make(chan struct{}),
		doorbellNow:   func() time.Time { return time.Unix(1_800_000_000, 0) },
		terminalWrite: func(string) error { *inputWrites++; return nil },
		attentionIsTTY: func() bool {
			return false
		},
		attentionWrite: func(data []byte) (int, error) {
			*attention = append(*attention, string(data))
			return len(data), nil
		},
	}
}

func TestSupersededWakeCannotEmitPeerAttention(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	writeWakeLockForTest(t, root, "codex", wakeLock{
		Generation: "incumbent",
		TTY:        "unknown",
	})

	inputWrites := 0
	var attention []string
	cfg := newGuardedWakeAttentionConfig(root, &inputWrites, &attention)
	cfg.terminalWrite = nil
	cfg.terminalGeneration = "incumbent"
	cfg.terminalTTY = "unknown"
	if !authorizeTerminalWritePlatform(cfg) {
		t.Fatal("test precondition: incumbent lock did not authorize terminal input")
	}

	writeWakeLockForTest(t, root, "codex", wakeLock{
		Generation: "replacement",
		TTY:        "unknown",
	})
	if authorizeTerminalWritePlatform(cfg) {
		t.Fatal("test precondition: replacement lock still authorized terminal input")
	}
	stubTIOCSTIInject(t, func(string) error {
		inputWrites++
		return nil
	})
	deliverPartialWakeMessageForTest(t, root, "codex", "replacement-race")

	err := notifyNewMessages(cfg)
	if !isWakeTerminalAuthorityLoss(err) {
		t.Fatalf("superseded wake result = %v, want terminal authority loss", err)
	}
	if inputWrites != 0 {
		t.Fatalf("superseded wake injected %d terminal chunks", inputWrites)
	}
	if len(attention) != 0 {
		t.Fatalf("superseded wake emitted peer attention: %#v", attention)
	}
	pending, readErr := os.ReadDir(fsq.AgentInboxNew(root, "codex"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got := len(pending); got != 1 {
		t.Fatalf("superseded wake changed durable inbox: %d messages", got)
	}
}
