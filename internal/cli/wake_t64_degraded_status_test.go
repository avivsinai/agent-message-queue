//go:build darwin || linux

package cli

import (
	"errors"
	"path/filepath"
	"testing"
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
// Bead agent-message-queue-oug: injectViaHook is the same legacy exit-zero
// result as the old shell injector, without a 5s process timeout.
func TestT64SuccessfulInjectDoesNotPersistDegraded(t *testing.T) {
	var recordedStatus string
	cfg := &wakeConfig{
		me:         "codex",
		root:       filepath.Join(secureTempDirForTest(t), "amq-root"),
		wakeOwner:  &wakeOwner{},
		injectMode: wakeInjectModePaste,
		injectVia:  "test-hook",
		injectViaHook: func(string) (string, error) {
			return "", nil
		},
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

	// Second delivery: legacy exit-zero inject, through the test hook so the
	// clear does not wait on a subprocess (bead agent-message-queue-oug).
	cfg.injectMode = wakeInjectModePaste
	cfg.injectVia = "test-hook"
	cfg.injectViaHook = func(string) (string, error) {
		return "", nil
	}

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

// TestT64FailedSpecificStatusWriteRetainsPendingIntent verifies that when
// recordNotifierStatus fails for a specific status (injector_unsupported),
// the attention-only fallback does not overwrite the pending intent with
// generic "degraded". The pending specific status must be retained.
func TestT64FailedSpecificStatusWriteRetainsPendingIntent(t *testing.T) {
	var statuses []struct{ status, mode, reason string }
	var failNext bool
	cfg := &wakeConfig{
		me:             "codex",
		root:           filepath.Join(secureTempDirForTest(t), "amq-root"),
		wakeOwner:      &wakeOwner{},
		injectMode:     wakeInjectModeNone,
		attentionIsTTY: func() bool { return false },
		attentionWrite: func(data []byte) (int, error) { return len(data), nil },
		recordNotifierStatus: func(status, mode, reason string) error {
			if failNext {
				return errors.New("simulated persistence failure")
			}
			statuses = append(statuses, struct{ status, mode, reason string }{status, mode, reason})
			return nil
		},
	}

	current := wakeDoorbellTestFiles(t, "pending.md")
	notice := peerWakeNotification("test message")

	// First: set up a specific status via the unsupported path.
	// We simulate this by directly persisting injector_unsupported, which fails,
	// leaving a pending intent. Then the attention-only fallback must not
	// overwrite it with degraded.
	failNext = true
	if err := persistWakeNotifierStatus(cfg, wakeInjectorUnsupportedStatus, "raw", "unsupported"); err == nil {
		t.Fatal("expected persistence failure")
	}
	if cfg.pendingNotifierStatus == nil || cfg.pendingNotifierStatus.status != wakeInjectorUnsupportedStatus {
		t.Fatalf("pending status = %+v, want injector_unsupported", cfg.pendingNotifierStatus)
	}

	// Now deliver via attention-only (injectMode=none).
	if err := deliverNewMessageNotification(cfg, notice, false, current); err != nil {
		t.Fatalf("delivery error = %v", err)
	}

	// The pending intent must still be injector_unsupported, not degraded.
	if cfg.pendingNotifierStatus != nil && cfg.pendingNotifierStatus.status == "degraded" {
		t.Fatalf("fallback overwrote pending injector_unsupported with degraded")
	}
	if cfg.pendingNotifierStatus != nil && cfg.pendingNotifierStatus.status != wakeInjectorUnsupportedStatus {
		t.Fatalf("pending status = %q, want injector_unsupported", cfg.pendingNotifierStatus.status)
	}
}

// TestT64NativeInjectionClearsDegradedAfterFallback verifies that after an
// attention-only fallback persists "degraded", a subsequent successful native
// injection (raw/paste mode, not external inject-via) clears it.
func TestT64NativeInjectionClearsDegradedAfterFallback(t *testing.T) {
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

	// First delivery: injectMode=none forces attention-only → "degraded"
	if err := deliverNewMessageNotification(cfg, notice, false, current); err != nil {
		t.Fatalf("first delivery error = %v", err)
	}
	if cfg.lastPersistedNotifierStatus != "degraded" {
		t.Fatalf("after fallback: status = %q, want \"degraded\"", cfg.lastPersistedNotifierStatus)
	}

	// Second delivery: switch to native raw mode with a terminalWrite that
	// always succeeds (simulates successful native TIOCSTI injection).
	cfg.injectMode = wakeInjectModeRaw
	cfg.terminalWrite = func(chunk string) error { return nil }
	cfg.beforeTerminalWrite = func() error { return nil }

	// Fresh pending files so the doorbell plan attempts delivery again
	current2 := wakeDoorbellTestFiles(t, "pending2.md")
	notice2 := peerWakeNotification("second message")
	if err := deliverNewMessageNotification(cfg, notice2, false, current2); err != nil {
		t.Fatalf("second delivery error = %v", err)
	}
	if cfg.lastPersistedNotifierStatus == "degraded" {
		t.Fatalf("after native inject: status still \"degraded\", want cleared")
	}
}
