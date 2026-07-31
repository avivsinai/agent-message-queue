//go:build darwin || linux

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

func TestMissingWakeGenerationCannotEmitPeerAttention(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	deliverPartialWakeMessageForTest(t, root, "codex", "missing-generation")

	inputWrites := 0
	var attention []string
	cfg := newGuardedWakeAttentionConfig(root, &inputWrites, &attention)
	cfg.terminalWrite = nil
	cfg.terminalGeneration = "missing"
	cfg.terminalTTY = "unknown"
	stubTIOCSTIInject(t, func(string) error {
		inputWrites++
		return nil
	})

	err := notifyNewMessages(cfg)
	if !isWakeTerminalAuthorityLoss(err) {
		t.Fatalf("missing-generation result = %v, want terminal authority loss", err)
	}
	if inputWrites != 0 {
		t.Fatalf("missing-generation wake injected %d terminal chunks", inputWrites)
	}
	if len(attention) != 0 {
		t.Fatalf("missing-generation wake emitted peer attention: %#v", attention)
	}
}

func TestRetainedAuthorityLossCannotEmitPeerAttention(t *testing.T) {
	root := secureTempDirForTest(t)
	deliverPartialWakeMessageForTest(t, root, "codex", "retained-loss")

	inputWrites := 0
	var attention []string
	cfg := newGuardedWakeAttentionConfig(root, &inputWrites, &attention)
	cfg.beforeTerminalWrite = func() error {
		return newWakeTerminalAuthorityLoss("wake generation changed")
	}

	err := notifyNewMessages(cfg)
	if !isWakeTerminalAuthorityLoss(err) {
		t.Fatalf("retained-authority result = %v, want terminal authority loss", err)
	}
	if inputWrites != 0 {
		t.Fatalf("lost retained authority wrote %d terminal chunks", inputWrites)
	}
	if len(attention) != 0 {
		t.Fatalf("lost retained authority emitted peer attention: %#v", attention)
	}
}

func TestInconclusiveWakeGenerationKeepsPeerAttention(t *testing.T) {
	root := secureTempDirForTest(t)
	deliverPartialWakeMessageForTest(t, root, "codex", "inconclusive-generation")

	inputWrites := 0
	var attention []string
	cfg := newGuardedWakeAttentionConfig(root, &inputWrites, &attention)
	cfg.beforeTerminalWrite = func() error {
		return newWakeTerminalTransientFailure(
			"inspect current wake generation",
			syscall.EMFILE,
		)
	}

	if err := notifyNewMessages(cfg); err != nil {
		t.Fatal(err)
	}
	if inputWrites != 0 ||
		len(attention) != 1 ||
		!strings.Contains(attention[0], "message from peer - inconclusive-generation") ||
		cfg.doorbell.attempts != 1 ||
		!cfg.doorbell.nextAttempt.Equal(
			cfg.doorbellNow().Add(wakeDoorbellAttentionRetryBase),
		) {
		t.Fatalf(
			"inconclusive ownership silenced or mixed channels: input=%d attention=%#v state=%#v",
			inputWrites,
			attention,
			cfg.doorbell,
		)
	}
}

func TestUnboundWakeAttentionDoesNotRequireGeneration(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*testing.T, string, *wakeConfig)
	}{
		{
			name: "explicit output-only",
			configure: func(_ *testing.T, _ string, cfg *wakeConfig) {
				cfg.injectMode = wakeInjectModeNone
				cfg.terminalGeneration = "missing"
			},
		},
		{
			name: "ownerless legacy without lock",
			configure: func(_ *testing.T, _ string, cfg *wakeConfig) {
				cfg.terminalWrite = nil
			},
		},
		{
			name: "malformed present lock",
			configure: func(t *testing.T, root string, cfg *wakeConfig) {
				t.Helper()
				cfg.terminalWrite = nil
				cfg.terminalGeneration = "incumbent"
				cfg.terminalTTY = "unknown"
				lockPath := filepath.Join(fsq.AgentBase(root, "codex"), ".wake.lock")
				if err := os.WriteFile(lockPath, []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			ensureCoopWakeMailboxForTest(t, root, "codex")
			deliverPartialWakeMessageForTest(t, root, "codex", "attention")

			inputWrites := 0
			var attention []string
			cfg := newGuardedWakeAttentionConfig(root, &inputWrites, &attention)
			test.configure(t, root, cfg)

			if err := notifyNewMessages(cfg); err != nil {
				t.Fatalf("unbound attention result = %v", err)
			}
			if inputWrites != 0 {
				t.Fatalf("unbound attention wrote %d terminal chunks", inputWrites)
			}
			if len(attention) != 1 ||
				!strings.Contains(attention[0], "message from peer - attention") {
				t.Fatalf("unbound attention = %#v", attention)
			}
		})
	}
}

func TestParseableNewerOwnerGenerationIsConclusiveForLegacyGate(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	lockPath := filepath.Join(fsq.AgentBase(root, "codex"), ".wake.lock")
	if err := os.WriteFile(
		lockPath,
		[]byte(`{"generation":"newer","owner_schema":2}`),
		wakeOwnerLockFileMode,
	); err != nil {
		t.Fatal(err)
	}

	inspection := inspectWakeLock(root, "codex")
	if inspection.Status != wakeLockUnverified ||
		inspection.Lock.Generation != "newer" {
		t.Fatalf("newer owner inspection = %#v", inspection)
	}
	allowed, err := authorizeTerminalWritePlatformState(&wakeConfig{
		root:               root,
		me:                 "codex",
		terminalGeneration: "incumbent",
		terminalTTY:        "unknown",
	})
	if allowed || !isWakeTerminalAuthorityLoss(err) {
		t.Fatalf("newer owner authorization = (%t, %v), want conclusive loss", allowed, err)
	}
}
