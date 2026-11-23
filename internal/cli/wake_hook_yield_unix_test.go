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

// The wake yields only when the hook covers every message of the cohort:
// a waiter holds the lock and no hook announced it, or a hook announced it
// less than hookRingGrace ago. Rows 2 and 4 to 6 come from review findings
// H9 and H14: an announced message that nobody drained left the waiter
// silent on it and the wake yielding to that waiter, so it was never
// announced again (a Stop-guard report and an inbox file not named
// <id>.md escaped the first fix). Rows 9 and 10 come from finding H17: mail
// that arrived in the turn a ring started, when no waiter runs, was typed
// into that running turn. Row 11 comes from H18: the wake yielded for an
// inbox file with no readable header, which the waiter cannot ring.
func TestHookDoorbellYieldsOnlyToCurrentDeliveryEvidence(t *testing.T) {
	type record struct {
		ids []string
		age time.Duration
	}
	type mail struct{ file, id string }
	one := []mail{{"mail.md", "mail"}}
	for _, tc := range []struct {
		name      string
		published wakeChannel
		locked    bool
		rang      *record
		blocked   *record
		inbox     []mail
		yield     bool
	}{
		{"1 waiter, mail not announced", wakeChannelHook, true, nil, nil, one, true},
		{"2 waiter, mail rung 3 minutes ago", wakeChannelHook, true, &record{[]string{"mail"}, 3 * time.Minute}, nil, one, false},
		{"3 waiter, mail rung 30 s ago", wakeChannelHook, true, &record{[]string{"mail"}, 30 * time.Second}, nil, one, true},
		{"4 waiter, mail reported by the Stop guard 3 minutes ago", wakeChannelHook, true, nil, &record{[]string{"mail"}, 3 * time.Minute}, one, false},
		{"5 waiter, one message covered and one stale", wakeChannelHook, true,
			&record{[]string{"fresh"}, 30 * time.Second}, &record{[]string{"stale"}, 3 * time.Minute},
			[]mail{{"fresh.md", "fresh"}, {"stale.md", "stale"}}, false},
		{"6 waiter, bridged file name rung 3 minutes ago", wakeChannelHook, true,
			&record{[]string{"mail"}, 3 * time.Minute}, nil, []mail{{"bridged-copy.md", "mail"}}, false},
		{"7 no waiter, mail rung 30 s ago", wakeChannelHook, false, &record{[]string{"mail"}, 30 * time.Second}, nil, one, true},
		{"8 no waiter, nothing announced", wakeChannelHook, false, nil, nil, one, false},
		{"9 no waiter, another message rung 30 s ago", wakeChannelHook, false, &record{[]string{"other"}, 30 * time.Second}, nil, one, true},
		{"10 no waiter, another message rung 3 minutes ago", wakeChannelHook, false, &record{[]string{"other"}, 3 * time.Minute}, nil, one, false},
		// An empty id stands for a file with no readable header.
		{"11 waiter, an inbox file with no readable header", wakeChannelHook, true, nil, nil, []mail{{"broken.md", ""}}, false},
		// Row 12 comes from finding H22, codex's live run T11 on 899a64b0:
		// the waiter wrote its rang record just after the wake's scan read
		// its clock, the negative age read as old, and the wake typed the
		// full doorbell 1.6 s after a fresh ring. Row 13 keeps the guard: a
		// time far ahead is not a fresh ring.
		{"12 no waiter, mail rung 50 ms after the scan's now", wakeChannelHook, false, &record{[]string{"mail"}, -50 * time.Millisecond}, nil, one, true},
		{"13 no waiter, mail rung 1 hour after the scan's now", wakeChannelHook, false, &record{[]string{"mail"}, -time.Hour}, nil, one, false},
		{"unpublished", "", true, nil, nil, one, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newWakeHoldHarness(t, wakeHoldPolicy{})
			h.cfg.publishedChannel = tc.published
			if tc.locked {
				defer holdWaiterForTest(t, h.root, "codex")()
			}
			agent := fsq.AgentBase(h.root, "codex")
			for path, rec := range map[string]*record{
				filepath.Join(agent, coopWaitStateFile): tc.rang,
				filepath.Join(agent, coopStopStateFile): tc.blocked,
			} {
				if rec == nil {
					continue
				}
				var state any = coopWaitState{Schema: 1, RangIDs: rec.ids}
				if filepath.Base(path) == coopStopStateFile {
					state = coopStopState{Schema: 1, BlockedIDs: rec.ids}
				}
				if err := writeCoopHookState(path, state); err != nil {
					t.Fatal(err)
				}
				at := h.now.Add(-rec.age)
				if err := os.Chtimes(path, at, at); err != nil {
					t.Fatal(err)
				}
			}
			for _, m := range tc.inbox {
				if m.id != "" {
					h.sendAs(m.file, m.id)
					continue
				}
				path, err := deliverToInboxForTest(t, h.root, "codex", m.file, []byte("not a message\n"))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(path, h.now, h.now); err != nil {
					t.Fatal(err)
				}
			}
			h.scan(h.cfg)
			want := 1
			if tc.yield {
				want = 0
			}
			if got := h.rings(); got != want {
				t.Fatalf("typed doorbells = %d, want %d", got, want)
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
	if _, held := h.cfg.hookYield(h.now, nil); held {
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
