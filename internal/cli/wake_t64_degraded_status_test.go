//go:build darwin || linux

package cli

import (
	"path/filepath"
	"testing"
	"time"
)

// TestT64AttentionOnlyFallbackPersistsDegraded verifies that when the wake
// falls back to attention-only delivery, persistWakeNotifierStatus is called
// with "degraded" so `amq doctor --ops` can distinguish a fully-injecting wake
// from a downgraded one.
// Bead agent-message-queue-t64.
func TestT64AttentionOnlyFallbackPersistsDegraded(t *testing.T) {
	var recordedStatus string
	var recordedReason string
	cfg := &wakeConfig{
		me:             "codex",
		root:           filepath.Join(secureTempDirForTest(t), "amq-root"),
		wakeOwner:      &wakeOwner{},
		injectMode:     wakeInjectModeNone, // forces attention-only fallback
		attentionIsTTY: func() bool { return false },
		attentionWrite: func(data []byte) (int, error) { return len(data), nil },
		recordNotifierStatus: func(status, mode, reason string) error {
			recordedStatus = status
			recordedReason = reason
			return nil
		},
		terminalGeneration: "",
	}

	current := wakeDoorbellTestFiles(t, "pending.md")
	notice := peerWakeNotification("test message")

	err := deliverNewMessageNotification(cfg, notice, false, current)
	if err != nil {
		t.Fatalf("delivery error = %v, want attention-only success", err)
	}

	if recordedStatus != "degraded" {
		t.Fatalf("notifier status = %q, want \"degraded\"", recordedStatus)
	}
	if recordedReason != "attention-only fallback" {
		t.Fatalf("notifier reason = %q, want \"attention-only fallback\"", recordedReason)
	}
}

// TestT64SuccessfulInjectDoesNotPersistDegraded verifies the negative: when
// injection succeeds, degraded status is NOT persisted (the naive fix that
// marks everything degraded would pass the test above but break this).
func TestT64SuccessfulInjectDoesNotPersistDegraded(t *testing.T) {
	injector := writeExecutableScriptForTest(t, "success-injector", "#!/bin/sh\nexit 0\n")

	var recordedStatus string
	cfg := &wakeConfig{
		me:             "codex",
		root:           filepath.Join(secureTempDirForTest(t), "amq-root"),
		wakeOwner:      &wakeOwner{},
		injectMode:     wakeInjectModePaste,
		injectVia:      injector,
		injectTimeout:  5 * time.Second,
		attentionIsTTY: func() bool { return false },
		attentionWrite: func(data []byte) (int, error) { return len(data), nil },
		recordNotifierStatus: func(status, mode, reason string) error {
			recordedStatus = status
			return nil
		},
		terminalGeneration: "",
	}

	current := wakeDoorbellTestFiles(t, "pending.md")
	notice := peerWakeNotification("test message")

	err := deliverNewMessageNotification(cfg, notice, false, current)
	if err != nil {
		t.Fatalf("delivery error = %v, want inject success", err)
	}

	if recordedStatus == "degraded" {
		t.Fatalf("notifier status = \"degraded\" after successful inject, want not degraded")
	}
}

// TestT64SuccessfulInjectClearsDegradedAfterFallback verifies the recovery
// transition codex requested: after an attention-only fallback persists
// "degraded", a subsequent successful inject clears it so doctor --ops
// reports healthy again.
func TestT64SuccessfulInjectClearsDegradedAfterFallback(t *testing.T) {
	// First delivery: injectMode=none forces attention-only → "degraded"
	var statuses []struct{ status, mode, reason string }
	cfg := &wakeConfig{
		me:             "codex",
		root:           filepath.Join(secureTempDirForTest(t), "amq-root"),
		wakeOwner:      &wakeOwner{},
		injectMode:     wakeInjectModeNone,
		attentionIsTTY: func() bool { return false },
		attentionWrite: func(data []byte) (int, error) { return len(data), nil },
		recordNotifierStatus: func(status, mode, reason string) error {
			statuses = append(statuses, struct{ status, mode, reason string }{status, mode, reason})
			return nil
		},
	}

	current := wakeDoorbellTestFiles(t, "pending.md")
	notice := peerWakeNotification("test message")

	if err := deliverNewMessageNotification(cfg, notice, false, current); err != nil {
		t.Fatalf("first delivery error = %v", err)
	}
	if cfg.lastPersistedNotifierStatus != "degraded" {
		t.Fatalf("after fallback: status = %q, want \"degraded\"", cfg.lastPersistedNotifierStatus)
	}

	// Second delivery: switch to a successful injector
	injector := writeExecutableScriptForTest(t, "success-injector", "#!/bin/sh\nexit 0\n")
	cfg.injectMode = wakeInjectModePaste
	cfg.injectVia = injector
	cfg.injectTimeout = 5 * time.Second

	// Fresh pending files so the doorbell plan attempts delivery again
	current2 := wakeDoorbellTestFiles(t, "pending2.md")
	notice2 := peerWakeNotification("second message")
	if err := deliverNewMessageNotification(cfg, notice2, false, current2); err != nil {
		t.Fatalf("second delivery error = %v, outcome=%v", err, cfg.lastInjectorOutcome)
	}
	t.Logf("after second delivery: outcome=%v status=%q lastAttemptAttention=%v", cfg.lastInjectorOutcome, cfg.lastPersistedNotifierStatus, cfg.lastAttemptAttention)
	if cfg.lastPersistedNotifierStatus == "degraded" {
		t.Fatalf("after successful inject: status still \"degraded\", want cleared")
	}
}
