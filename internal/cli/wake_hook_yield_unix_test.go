//go:build darwin || linux

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/notificationattempt"
)

func holdWaiterForTest(t *testing.T, root, me string) func() {
	t.Helper()
	release, held, err := lock.TryExclusive(filepath.Join(fsq.AgentBase(root, me), coopWaitLockFile))
	if err != nil || !held {
		t.Fatalf("take waiter lock: held=%t, err=%v", held, err)
	}
	return release
}

func TestHookDoorbellYieldsOnlyToCurrentDeliveryEvidence(t *testing.T) {
	for _, tc := range []struct {
		name      string
		published wakeChannel
		locked    bool
		ringAge   time.Duration
		wait      time.Duration
	}{
		{"live waiter", wakeChannelHook, true, 0, wakeDoorbellRetryBase},
		{"recent ring", wakeChannelHook, false, hookRingGrace - time.Second, time.Second},
		{"expired ring", wakeChannelHook, false, hookRingGrace, 0},
		{"unpublished", "", true, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newWakeHoldHarness(t, wakeHoldPolicy{})
			h.cfg.publishedChannel = tc.published
			if tc.locked {
				defer holdWaiterForTest(t, h.root, "codex")()
			}
			if tc.ringAge != 0 {
				path := filepath.Join(fsq.AgentBase(h.root, "codex"), coopWaitStateFile)
				if err := writeCoopHookState(path, coopWaitState{Schema: 1, RangIDs: []string{"mail"}}); err != nil {
					t.Fatal(err)
				}
				at := h.now.Add(-tc.ringAge)
				if err := os.Chtimes(path, at, at); err != nil {
					t.Fatal(err)
				}
			}
			wait, held := h.cfg.hookYield(h.now)
			if wait != tc.wait || held != (tc.wait > 0) {
				t.Fatalf("hold = %v, %t; want %v", wait, held, tc.wait)
			}
		})
	}
}

func TestHookDoorbellResumesTypingWhenTheWaiterEnds(t *testing.T) {
	h := newWakeHoldHarness(t, wakeHoldPolicy{})
	h.cfg.settings.hookDoorbell = true
	h.cfg.recordSettingsApplied = func(wakeSettingsAppliedStatus) error { return errors.New("write failed") }
	noteWakeSettingsApplied(h.cfg, wakeSettingsAppliedStatus{})
	release := holdWaiterForTest(t, h.root, "codex")
	if _, held := h.cfg.hookYield(h.now); held {
		release()
		t.Fatal("yielded before the channel was published")
	}
	h.cfg.recordSettingsApplied = func(wakeSettingsAppliedStatus) error { return nil }
	if err := retryPendingWakeSettingsApplied(h.cfg); err != nil {
		release()
		t.Fatal(err)
	}
	h.send("a", format.PriorityNormal)
	h.scan(h.cfg)
	release()
	if len(h.injected) != 0 {
		t.Fatalf("terminal writes while held: %q", h.injected)
	}
	if attempts, err := notificationattempt.List(h.root, "codex", ""); err != nil || len(attempts) != 0 {
		t.Fatalf("ledger while held: %v, %v", attempts, err)
	}
	h.advance(hookRingGrace)
	h.scan(h.cfg)
	if got := h.rings(); got != 1 {
		t.Fatalf("doorbells after release = %d, want 1", got)
	}
}
