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

func TestSameGenerationTTYDriftKeepsPeerAttention(t *testing.T) {
	tests := []struct {
		name    string
		lockTTY string
		cfgTTY  string
	}{
		{
			name:    "different TTY",
			lockTTY: "replacement-tty",
			cfgTTY:  "incumbent-tty",
		},
		{
			name:    "empty lock TTY evidence",
			lockTTY: "",
			cfgTTY:  "incumbent-tty",
		},
		{
			name:    "empty retained TTY evidence",
			lockTTY: "incumbent-tty",
			cfgTTY:  "",
		},
		{
			name:    "both TTY fields empty",
			lockTTY: "",
			cfgTTY:  "",
		},
		{
			name:    "both TTY fields whitespace",
			lockTTY: " ",
			cfgTTY:  " ",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			ensureCoopWakeMailboxForTest(t, root, "codex")
			writeWakeLockForTest(t, root, "codex", wakeLock{
				Generation: "incumbent",
				TTY:        test.lockTTY,
			})
			deliverPartialWakeMessageForTest(t, root, "codex", "same-generation")

			inputWrites := 0
			var attention []string
			cfg := newGuardedWakeAttentionConfig(root, &inputWrites, &attention)
			cfg.terminalWrite = nil
			cfg.terminalGeneration = "incumbent"
			cfg.terminalTTY = test.cfgTTY
			stubTIOCSTIInject(t, func(string) error {
				inputWrites++
				return nil
			})

			if err := notifyNewMessages(cfg); err != nil {
				t.Fatalf("same-generation TTY drift result = %v, want attention retry", err)
			}
			if inputWrites != 0 {
				t.Fatalf("same-generation TTY drift injected %d terminal chunks", inputWrites)
			}
			if len(attention) != 1 ||
				!strings.Contains(attention[0], "message from peer - same-generation") {
				t.Fatalf(
					"same-generation TTY drift attention = %#v, want one notice",
					attention,
				)
			}
		})
	}
}

func TestWhitespaceWakeGenerationCannotAuthorizeInputOrSilenceAttention(t *testing.T) {
	tests := []struct {
		name        string
		lockGen     string
		retainedGen string
	}{
		{
			name:        "whitespace lock generation",
			lockGen:     " ",
			retainedGen: "incumbent",
		},
		{
			name:        "both generations whitespace",
			lockGen:     " ",
			retainedGen: " ",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			ensureCoopWakeMailboxForTest(t, root, "codex")
			writeWakeLockForTest(t, root, "codex", wakeLock{
				Generation: test.lockGen,
				TTY:        "unknown",
			})
			deliverPartialWakeMessageForTest(t, root, "codex", "whitespace-generation")

			inputWrites := 0
			var attention []string
			cfg := newGuardedWakeAttentionConfig(root, &inputWrites, &attention)
			cfg.terminalWrite = nil
			cfg.terminalGeneration = test.retainedGen
			cfg.terminalTTY = "unknown"
			stubTIOCSTIInject(t, func(string) error {
				inputWrites++
				return nil
			})

			if err := notifyNewMessages(cfg); err != nil {
				t.Fatalf("whitespace generation result = %v, want attention retry", err)
			}
			if inputWrites != 0 {
				t.Fatalf("whitespace generation injected %d terminal chunks", inputWrites)
			}
			if len(attention) != 1 ||
				!strings.Contains(attention[0], "message from peer - whitespace-generation") {
				t.Fatalf("whitespace generation attention = %#v", attention)
			}
		})
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

func TestBoundOutputOnlyWakeStopsAfterGenerationSupersession(t *testing.T) {
	modes := []struct {
		name      string
		configure func(*testing.T, *wakeConfig)
	}{
		{
			name: "explicit output-only",
			configure: func(_ *testing.T, cfg *wakeConfig) {
				cfg.injectMode = wakeInjectModeNone
			},
		},
		{
			name: "input-demoted",
			configure: func(t *testing.T, cfg *wakeConfig) {
				t.Helper()
				if err := disableWakeInput(cfg, nil); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	supersessions := []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{
			name: "generation disappeared",
			setup: func(*testing.T, string) {
				// A retained prior generation plus ENOENT is conclusive.
			},
		},
		{
			name: "generation replaced",
			setup: func(t *testing.T, root string) {
				t.Helper()
				writeWakeLockForTest(t, root, "codex", wakeLock{
					Generation: "replacement",
					TTY:        "unknown",
				})
			},
		},
	}

	for _, mode := range modes {
		for _, supersession := range supersessions {
			t.Run(mode.name+"/"+supersession.name, func(t *testing.T) {
				root := secureTempDirForTest(t)
				ensureCoopWakeMailboxForTest(t, root, "codex")
				supersession.setup(t, root)
				deliverPartialWakeMessageForTest(t, root, "codex", "superseded-output")

				inputWrites := 0
				var attention []string
				cfg := newGuardedWakeAttentionConfig(root, &inputWrites, &attention)
				cfg.terminalGeneration = "incumbent"
				cfg.terminalTTY = "unknown"
				mode.configure(t, cfg)

				err := notifyNewMessages(cfg)
				if !isWakeTerminalAuthorityLoss(err) {
					t.Fatalf(
						"bound output-only supersession result = %v, want typed loss",
						err,
					)
				}
				if inputWrites != 0 {
					t.Fatalf("bound output-only wake injected %d terminal chunks", inputWrites)
				}
				if len(attention) != 0 {
					t.Fatalf(
						"bound output-only wake emitted peer attention: %#v",
						attention,
					)
				}
				if got := classifyWakeFailure(err); got != wakeFailureFatal {
					t.Fatalf("supersession disposition = %d, want fatal", got)
				}
			})
		}
	}
}

func TestBoundOutputOnlyWakeKeepsAttentionOnInconclusiveGeneration(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{
			name: "empty generation",
			setup: func(t *testing.T, root string) {
				t.Helper()
				writeWakeLockForTest(t, root, "codex", wakeLock{
					TTY: "unknown",
				})
			},
		},
		{
			name: "malformed lock",
			setup: func(t *testing.T, root string) {
				t.Helper()
				lockPath := filepath.Join(fsq.AgentBase(root, "codex"), ".wake.lock")
				if err := os.WriteFile(lockPath, []byte("{"), wakeOwnerLockFileMode); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "same generation TTY drift",
			setup: func(t *testing.T, root string) {
				t.Helper()
				writeWakeLockForTest(t, root, "codex", wakeLock{
					Generation: "incumbent",
					TTY:        "replacement-tty",
				})
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			ensureCoopWakeMailboxForTest(t, root, "codex")
			test.setup(t, root)
			deliverPartialWakeMessageForTest(t, root, "codex", "inconclusive-output")

			inputWrites := 0
			var attention []string
			cfg := newGuardedWakeAttentionConfig(root, &inputWrites, &attention)
			cfg.injectMode = wakeInjectModeNone
			cfg.terminalGeneration = "incumbent"
			cfg.terminalTTY = "unknown"

			if err := notifyNewMessages(cfg); err != nil {
				t.Fatalf("inconclusive output-only result = %v", err)
			}
			if inputWrites != 0 {
				t.Fatalf("inconclusive output-only wake injected %d chunks", inputWrites)
			}
			if len(attention) != 1 ||
				!strings.Contains(attention[0], "message from peer - inconclusive-output") {
				t.Fatalf("inconclusive output-only attention = %#v", attention)
			}
		})
	}
}

func TestRunWakeLoopBoundOutputOnlyExitsAfterGenerationSupersession(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	attentionWrites := 0

	err := runWakeLoop(wakeConfig{
		root:               root,
		me:                 "codex",
		session:            "session1",
		injectMode:         wakeInjectModeNone,
		controlStop:        make(chan struct{}),
		terminalGeneration: "incumbent",
		terminalTTY:        "unknown",
		attentionIsTTY:     func() bool { return false },
		attentionWrite: func(data []byte) (int, error) {
			attentionWrites++
			return len(data), nil
		},
		onPrepared: func(wakeAdmissionWatcher) error {
			deliverPartialWakeMessageForTest(
				t,
				root,
				"codex",
				"superseded-output-loop",
			)
			return nil
		},
	})
	if !isWakeTerminalAuthorityLoss(err) {
		t.Fatalf("bound output-only wake exit = %v, want typed authority loss", err)
	}
	if attentionWrites != 0 {
		t.Fatalf("superseded output-only loop emitted %d peer notices", attentionWrites)
	}
}

func TestBoundWakeSelfDiagnosticAttentionIgnoresGenerationFence(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	var attention []string
	cfg := newGuardedWakeAttentionConfig(root, new(int), &attention)
	cfg.injectMode = wakeInjectModeNone
	cfg.terminalGeneration = "missing"
	cfg.terminalTTY = "unknown"

	if err := deliverWakeAttentionOnly(cfg, wakePayload{
		text:       "wake lock unreadable; injection paused",
		provenance: wakePayloadSystemFixed,
	}); err != nil {
		t.Fatalf("self-diagnostic attention: %v", err)
	}
	if len(attention) != 1 ||
		!strings.Contains(attention[0], "wake lock unreadable; injection paused") {
		t.Fatalf("self-diagnostic attention = %#v", attention)
	}
}

func TestWhitespaceRetainedGenerationDoesNotFenceOutputOnlyAttention(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	deliverPartialWakeMessageForTest(t, root, "codex", "no-prior-generation")

	var attention []string
	cfg := newGuardedWakeAttentionConfig(root, new(int), &attention)
	cfg.injectMode = wakeInjectModeNone
	cfg.terminalGeneration = " "
	cfg.terminalTTY = "unknown"

	if err := notifyNewMessages(cfg); err != nil {
		t.Fatalf("whitespace retained generation result = %v", err)
	}
	if len(attention) != 1 ||
		!strings.Contains(attention[0], "message from peer - no-prior-generation") {
		t.Fatalf("whitespace retained generation attention = %#v", attention)
	}
}

func TestBoundWakeOperatorAttentionStopsAfterGenerationSupersession(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	var attention []string
	cfg := newGuardedWakeAttentionConfig(root, new(int), &attention)
	cfg.injectMode = wakeInjectModeNone
	cfg.terminalGeneration = "missing"
	cfg.terminalTTY = "unknown"

	err := deliverWakeAttentionOnly(cfg, wakePayload{
		text:       "operator-authored interrupt notice",
		provenance: wakePayloadOperatorFlag,
	})
	if !isWakeTerminalAuthorityLoss(err) {
		t.Fatalf("operator attention result = %v, want typed authority loss", err)
	}
	if len(attention) != 0 {
		t.Fatalf("superseded wake emitted operator attention: %#v", attention)
	}
}

func TestBoundTransientPeerAttentionStopsAfterGenerationSupersession(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	var attention []string
	cfg := newGuardedWakeAttentionConfig(root, new(int), &attention)
	cfg.terminalGeneration = "missing"
	cfg.terminalTTY = "unknown"

	err := deliverWakeTransientAttention(cfg, wakePayload{
		text:       "message from peer - transient",
		provenance: wakePayloadPeerHeaders,
	}, nil)
	if !isWakeTerminalAuthorityLoss(err) {
		t.Fatalf("transient peer attention result = %v, want typed authority loss", err)
	}
	if len(attention) != 0 {
		t.Fatalf("superseded wake emitted transient peer attention: %#v", attention)
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
