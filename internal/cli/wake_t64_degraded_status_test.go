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
