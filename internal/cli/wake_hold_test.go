package cli

// Purpose: guards hold-by-priority in the wake doorbell (--hold-normal,
// --hold-low). Each test drives notifyNewMessages with a fake clock and a fake
// terminal writer against a real inbox. They break if a normal burst rings
// more than once or later than first arrival + hold, if a drain does not
// cancel a held doorbell, if urgent mail waits behind a hold or a parked
// cohort, if a restart restarts the hold, or if zero holds change today's
// schedule.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type wakeHoldHarness struct {
	t        *testing.T
	root     string
	now      time.Time
	injected []string
	cfg      *wakeConfig
}

func newWakeHoldHarness(t *testing.T, policy wakeHoldPolicy) *wakeHoldHarness {
	t.Helper()
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	stubRawInputDrained(t, func(time.Duration, time.Duration) (time.Duration, bool, error) {
		return 0, true, nil
	})
	stubRawInjectSleep(t)
	h := &wakeHoldHarness{t: t, root: root, now: time.Unix(1_800_000_000, 0)}
	h.cfg = h.newConfig(policy)
	return h
}

// newConfig is a fresh waker on the same inbox: a restart.
func (h *wakeHoldHarness) newConfig(policy wakeHoldPolicy) *wakeConfig {
	return &wakeConfig{
		me:             "codex",
		root:           h.root,
		session:        "s",
		wakeOwner:      &wakeOwner{},
		injectMode:     wakeInjectModeRaw,
		settings:       wakeSettings{holdNormal: policy.normal, holdLow: policy.low},
		doorbellNow:    func() time.Time { return h.now },
		attentionIsTTY: func() bool { return false },
		terminalWrite: func(text string) error {
			h.injected = append(h.injected, text)
			return nil
		},
	}
}

// send delivers a message whose arrival (file mtime) is the fake now. An
// empty priority leaves the header without one.
func (h *wakeHoldHarness) send(name, priority string) {
	h.t.Helper()
	msg := format.Message{
		Header: format.Header{
			Schema: 1, ID: name, From: "peer", To: []string{"codex"},
			Thread: "p2p/codex__peer", Subject: name, Priority: priority,
			Created: "2026-07-28T15:00:00Z",
		},
		Body: "body",
	}
	data, err := msg.Marshal()
	if err != nil {
		h.t.Fatal(err)
	}
	path, err := deliverToInboxForTest(h.t, h.root, "codex", name+".md", data)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := os.Chtimes(path, h.now, h.now); err != nil {
		h.t.Fatal(err)
	}
}

func (h *wakeHoldHarness) scan(cfg *wakeConfig) {
	h.t.Helper()
	if err := notifyNewMessages(cfg); err != nil {
		h.t.Fatalf("notifyNewMessages: %v", err)
	}
}

// rings returns the doorbells typed since the last call.
func (h *wakeHoldHarness) rings() int {
	n := strings.Count(strings.Join(h.injected, "|"), coopWakeDoorbell)
	h.injected = nil
	return n
}

func (h *wakeHoldHarness) advance(d time.Duration) { h.now = h.now.Add(d) }

func (h *wakeHoldHarness) drain() {
	h.t.Helper()
	runDrainJSON(h.t, h.root, "codex", 100, false)
}

func (h *wakeHoldHarness) deadline() time.Time {
	h.t.Helper()
	deadline, ok := h.cfg.doorbell.nextDeadline()
	if !ok {
		h.t.Fatal("no doorbell deadline is set")
	}
	return deadline
}

var wakeHoldRecommended = wakeHoldPolicy{normal: 5 * time.Minute, low: 30 * time.Minute}

func TestWakeHoldNormalBurstRingsOnceAtFirstDeadline(t *testing.T) {
	h := newWakeHoldHarness(t, wakeHoldRecommended)
	start := h.now
	h.send("a", format.PriorityNormal)
	h.scan(h.cfg)
	h.advance(time.Minute)
	h.send("b", format.PriorityNormal)
	h.scan(h.cfg)
	h.advance(time.Minute)
	h.send("c", "") // missing priority counts as normal
	h.scan(h.cfg)
	if got := h.rings(); got != 0 {
		t.Fatalf("rings before the deadline = %d, want 0", got)
	}
	if got, want := h.deadline(), start.Add(5*time.Minute); !got.Equal(want) {
		t.Fatalf("deadline = %v, want first arrival + 5m = %v", got, want)
	}

	h.now = start.Add(5*time.Minute - time.Second)
	h.scan(h.cfg)
	if got := h.rings(); got != 0 {
		t.Fatalf("rings one second early = %d, want 0", got)
	}
	h.now = start.Add(5 * time.Minute)
	h.scan(h.cfg)
	if got := h.rings(); got != 1 {
		t.Fatalf("rings at the deadline = %d, want exactly 1", got)
	}
}

func TestWakeHoldLowRingsAtItsDeadlineAndNormalPullsItEarlier(t *testing.T) {
	h := newWakeHoldHarness(t, wakeHoldRecommended)
	start := h.now
	h.send("low", format.PriorityLow)
	h.scan(h.cfg)
	if got, want := h.deadline(), start.Add(30*time.Minute); !got.Equal(want) {
		t.Fatalf("low deadline = %v, want %v", got, want)
	}
	h.advance(10 * time.Minute)
	h.send("normal", format.PriorityNormal)
	h.scan(h.cfg)
	if got, want := h.deadline(), start.Add(15*time.Minute); !got.Equal(want) {
		t.Fatalf("deadline after a normal message = %v, want pulled earlier to %v", got, want)
	}
	h.advance(5 * time.Minute)
	h.scan(h.cfg)
	if got := h.rings(); got != 1 {
		t.Fatalf("rings at the pulled deadline = %d, want 1", got)
	}

	// Low-only mail still rings at its own finite deadline.
	h2 := newWakeHoldHarness(t, wakeHoldRecommended)
	h2.send("low", format.PriorityLow)
	h2.scan(h2.cfg)
	h2.advance(30*time.Minute - time.Second)
	h2.scan(h2.cfg)
	if got := h2.rings(); got != 0 {
		t.Fatalf("low mail rang early: %d", got)
	}
	h2.advance(time.Second)
	h2.scan(h2.cfg)
	if got := h2.rings(); got != 1 {
		t.Fatalf("low mail rings at its deadline = %d, want 1", got)
	}
}

func TestWakeHoldNormalDoesNotPullALowerHoldLater(t *testing.T) {
	h := newWakeHoldHarness(t, wakeHoldRecommended)
	start := h.now
	h.send("normal", format.PriorityNormal)
	h.scan(h.cfg)
	h.advance(time.Minute)
	h.send("low", format.PriorityLow)
	h.scan(h.cfg)
	if got, want := h.deadline(), start.Add(5*time.Minute); !got.Equal(want) {
		t.Fatalf("a later low message moved the deadline to %v, want %v", got, want)
	}
}

func TestWakeHoldDrainBeforeDeadlineCancelsDoorbell(t *testing.T) {
	h := newWakeHoldHarness(t, wakeHoldRecommended)
	h.send("a", format.PriorityNormal)
	h.scan(h.cfg)
	h.advance(2 * time.Minute)
	h.drain()
	h.scan(h.cfg)
	if _, ok := h.cfg.doorbell.nextDeadline(); ok {
		t.Fatal("a drained inbox kept a doorbell deadline")
	}
	h.advance(10 * time.Minute)
	h.scan(h.cfg)
	if got := h.rings(); got != 0 {
		t.Fatalf("rings after a drain = %d, want 0", got)
	}
}

func TestWakeHoldUrgentRingsAtOnceDuringAHold(t *testing.T) {
	h := newWakeHoldHarness(t, wakeHoldRecommended)
	h.send("a", format.PriorityNormal)
	h.scan(h.cfg)
	h.advance(time.Minute)
	h.send("verdict", format.PriorityUrgent)
	h.scan(h.cfg)
	if got := h.rings(); got != 1 {
		t.Fatalf("rings for urgent during a normal hold = %d, want 1 at once", got)
	}
}

// Today an addition behind a parked cohort revives it only while the lifetime
// cap allows. Urgent mail must ring even when the budget is used up, and the
// same urgent message seen again must not ring again.
func TestWakeHoldUrgentRingsBehindParkedCohortWithExhaustedBudget(t *testing.T) {
	h := newWakeHoldHarness(t, wakeHoldRecommended)
	h.send("old", format.PriorityUrgent)
	h.scan(h.cfg)
	h.rings()
	// Park the cohort, then spend the whole lifetime budget on additions.
	for i := uint(0); h.cfg.doorbell.phase != wakeDoorbellParked && i < 20; i++ {
		h.advance(wakeDoorbellRetryMax)
		h.scan(h.cfg)
		h.rings()
	}
	if h.cfg.doorbell.phase != wakeDoorbellParked {
		t.Fatalf("setup: phase = %v, want parked", h.cfg.doorbell.phase)
	}
	h.cfg.doorbell.attemptBudget = wakeDoorbellLifetimeAttemptCap

	h.advance(time.Second)
	h.send("fresh", format.PriorityUrgent)
	h.scan(h.cfg)
	if got := h.rings(); got != 1 {
		t.Fatalf("rings for urgent behind an exhausted parked cohort = %d, want 1", got)
	}
	for i := 0; i < 20; i++ {
		h.advance(wakeDoorbellRetryMax)
		h.scan(h.cfg)
	}
	if got := h.rings(); got > 1 {
		t.Fatalf("a re-seen urgent message rang %d more times, want at most its one ladder retry", got)
	}
	if h.cfg.doorbell.phase != wakeDoorbellParked {
		t.Fatalf("phase after the ladder = %v, want parked again", h.cfg.doorbell.phase)
	}
}

func TestWakeHoldRestartKeepsOriginalArrival(t *testing.T) {
	h := newWakeHoldHarness(t, wakeHoldRecommended)
	start := h.now
	h.send("a", format.PriorityNormal)
	h.scan(h.cfg)
	h.advance(4 * time.Minute)

	restarted := h.newConfig(wakeHoldRecommended)
	h.scan(restarted)
	if got := h.rings(); got != 0 {
		t.Fatalf("rings right after restart = %d, want 0", got)
	}
	h.cfg = restarted
	if got, want := h.deadline(), start.Add(5*time.Minute); !got.Equal(want) {
		t.Fatalf("deadline after restart = %v, want original %v (not a new full hold)", got, want)
	}
	h.advance(time.Minute)
	h.scan(restarted)
	if got := h.rings(); got != 1 {
		t.Fatalf("rings at the original deadline = %d, want 1", got)
	}
}

// With both holds at 0 the schedule is today's: the first message rings at
// once, an addition is coalesced into the retry ladder, and the ladder retries
// at 5s.
func TestWakeHoldZeroPolicyKeepsTodaysSchedule(t *testing.T) {
	h := newWakeHoldHarness(t, wakeHoldPolicy{})
	h.send("a", format.PriorityLow)
	h.scan(h.cfg)
	if got := h.rings(); got != 1 {
		t.Fatalf("first message rings = %d, want 1 at once", got)
	}
	h.advance(time.Second)
	h.send("b", format.PriorityNormal)
	h.scan(h.cfg)
	if got := h.rings(); got != 0 {
		t.Fatalf("addition rang again inside the ladder: %d", got)
	}
	if got, want := h.deadline(), h.now.Add(wakeDoorbellRetryBase-time.Second); !got.Equal(want) {
		t.Fatalf("retry deadline = %v, want %v", got, want)
	}
	h.advance(wakeDoorbellRetryBase)
	h.scan(h.cfg)
	if got := h.rings(); got != 1 {
		t.Fatalf("retry rings = %d, want 1", got)
	}
}

// A file dated in the future has no trustworthy arrival time. Clamping it to
// the scan time moved the deadline later at every waker restart, so the
// doorbell could be postponed forever. Such mail gets no hold: it rings at once
// and a restart mid-hold rings again at once.
func TestWakeHoldFutureMtimeIsNeverPostponedByRestart(t *testing.T) {
	h := newWakeHoldHarness(t, wakeHoldRecommended)
	start := h.now
	h.send("a", format.PriorityNormal)
	future := start.Add(time.Hour)
	path := filepath.Join(h.root, "agents", "codex", "inbox", "new", "a.md")
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	h.scan(h.cfg)
	if got := h.rings(); got != 1 {
		t.Fatalf("rings for a future-dated normal message = %d, want 1 at once", got)
	}

	h.advance(4 * time.Minute)
	restarted := h.newConfig(wakeHoldRecommended)
	h.scan(restarted)
	if got := h.rings(); got != 1 {
		t.Fatalf("rings after a mid-hold restart = %d, want 1 at once, not postponed", got)
	}
}

// PR #1008 review: a parked cohort revived by low mail stored the hold in
// the retry deadline. A later normal arrival could then wait 24 minutes past
// its own deadline because held additions did not pull the retry ladder forward.
func TestWakeHoldParkedAdditionCanPullHoldEarlier(t *testing.T) {
	h := newWakeHoldHarness(t, wakeHoldRecommended)
	h.send("old", format.PriorityUrgent)
	h.scan(h.cfg)
	for i := 0; h.cfg.doorbell.phase != wakeDoorbellParked && i < 20; i++ {
		h.advance(wakeDoorbellRetryMax)
		h.scan(h.cfg)
	}
	if h.cfg.doorbell.phase != wakeDoorbellParked {
		t.Fatal("setup: cohort did not park")
	}
	h.rings()
	h.send("low", format.PriorityLow)
	h.scan(h.cfg)
	h.advance(time.Minute)
	normalArrival := h.now
	h.send("normal", format.PriorityNormal)
	h.scan(h.cfg)
	if got, want := h.deadline(), normalArrival.Add(5*time.Minute); !got.Equal(want) {
		t.Fatalf("deadline = %v, want normal arrival + hold = %v", got, want)
	}
	h.now = normalArrival.Add(5*time.Minute - time.Second)
	h.scan(h.cfg)
	if got := h.rings(); got != 0 {
		t.Fatalf("rings before normal deadline = %d, want 0", got)
	}
	h.advance(time.Second)
	h.scan(h.cfg)
	if got := h.rings(); got != 1 {
		t.Fatalf("rings at normal deadline = %d, want 1", got)
	}
}

// PR #1008 review: the announced-cohort hold lookup skipped filenames even
// when they identified a new physical message, so a replacement bypassed its hold.
func TestWakeHoldAnnouncedReplacementUsesNewArrival(t *testing.T) {
	h := newWakeHoldHarness(t, wakeHoldRecommended)
	h.cfg.retryUntil = wakeRetryUntilInjected
	h.send("same", format.PriorityUrgent)
	h.scan(h.cfg)
	h.rings()
	// The inbox drains and a new physical file arrives before the next scan.
	h.drain()
	h.advance(time.Minute)
	h.send("same", format.PriorityNormal)
	h.scan(h.cfg)
	if got := h.rings(); got != 0 {
		t.Fatalf("rings for fresh normal replacement = %d, want 0", got)
	}
	if got, want := h.deadline(), h.now.Add(5*time.Minute); !got.Equal(want) {
		t.Fatalf("replacement deadline = %v, want %v", got, want)
	}
	h.advance(5 * time.Minute)
	h.scan(h.cfg)
	if got := h.rings(); got != 1 {
		t.Fatalf("rings at replacement deadline = %d, want 1", got)
	}
}

// useSettingsFile makes the harness wake read .wake.settings from memory: the
// settingsSource seam the running wake reads through its agent directory.
// The harness has no terminal to sample, so files here turn input deferral
// off; it is not under test.
func (h *wakeHoldHarness) useSettingsFile(raw string) {
	h.cfg.settingsSource = func() ([]byte, bool, error) {
		return []byte(raw), true, nil
	}
}

// #1014: a live settings change applies to the running wake. A hold set in
// the file holds the next normal message; a hold removed from the file rings
// an already-held cohort at the next scan.
func TestWakeSettingsLiveHoldChange(t *testing.T) {
	// Review finding (live-apply P2): a read that a concurrent wake config
	// rename changed was logged and recorded as a refused file.
	t.Run("read changed by a concurrent write is no observation", func(t *testing.T) {
		h := newWakeHoldHarness(t, wakeHoldRecommended)
		h.cfg.settingsSource = func() ([]byte, bool, error) {
			return nil, false, newWakeSnapshotReadChangedError(errors.New("wake settings changed while opening"))
		}
		var recorded []wakeSettingsAppliedStatus
		h.cfg.recordSettingsApplied = func(status wakeSettingsAppliedStatus) error {
			recorded = append(recorded, status)
			return nil
		}
		if h.cfg.reloadSettings() || len(recorded) != 0 || h.cfg.settingsObserved.seen {
			t.Fatalf("recorded = %+v, observed = %+v; want no observation", recorded, h.cfg.settingsObserved)
		}
	})

	t.Run("hold set live holds the next message", func(t *testing.T) {
		h := newWakeHoldHarness(t, wakeHoldPolicy{})
		h.useSettingsFile(`{"schema":1,"settings":{"hold_normal":"5m","defer_while_input":false}}`)
		start := h.now
		h.send("a", format.PriorityNormal)
		h.scan(h.cfg)
		if got := h.rings(); got != 0 {
			t.Fatalf("rings before the live hold = %d, want 0", got)
		}
		h.now = start.Add(5*time.Minute - time.Second)
		h.scan(h.cfg)
		if got := h.rings(); got != 0 {
			t.Fatalf("rings one second early = %d, want 0", got)
		}
		h.now = start.Add(5 * time.Minute)
		h.scan(h.cfg)
		if got := h.rings(); got != 1 {
			t.Fatalf("rings at the live hold = %d, want 1", got)
		}
	})

	t.Run("hold removed live rings a held cohort", func(t *testing.T) {
		h := newWakeHoldHarness(t, wakeHoldRecommended)
		h.send("low", format.PriorityLow)
		h.scan(h.cfg)
		h.advance(time.Minute)
		h.scan(h.cfg)
		if got := h.rings(); got != 0 {
			t.Fatalf("setup: low mail rang inside its 30m hold: %d", got)
		}
		h.useSettingsFile(`{"schema":1,"settings":{"defer_while_input":false}}`)
		if !h.cfg.reloadSettings() {
			t.Fatal("reloadSettings did not report the change")
		}
		h.scan(h.cfg)
		if got := h.rings(); got != 1 {
			t.Fatalf("rings after holds were removed = %d, want 1 at once", got)
		}
	})

	// Design review (precedence races P1): the re-time after a policy change
	// counted the messages an announced cohort already covered, so a held
	// addition rang at once after any policy change.
	t.Run("announced cohort addition keeps its hold", func(t *testing.T) {
		h := newWakeHoldHarness(t, wakeHoldRecommended)
		h.cfg.retryUntil = wakeRetryUntilInjected
		h.send("old", format.PriorityUrgent)
		h.scan(h.cfg)
		if got := h.rings(); got != 1 {
			t.Fatalf("setup: urgent rings = %d, want 1", got)
		}
		h.advance(time.Minute)
		arrival := h.now
		h.send("new", format.PriorityNormal)
		h.scan(h.cfg)
		h.useSettingsFile(`{"schema":1,"settings":{"hold_normal":"5m","hold_low":"1h","defer_while_input":false}}`)
		if !h.cfg.reloadSettings() {
			t.Fatal("reloadSettings did not report the change")
		}
		h.scan(h.cfg)
		if got := h.rings(); got != 0 {
			t.Fatalf("rings after a policy change that keeps the normal hold = %d, want 0", got)
		}
		if got, want := h.deadline(), arrival.Add(5*time.Minute); !got.Equal(want) {
			t.Fatalf("deadline = %v, want addition arrival + 5m = %v", got, want)
		}
		h.now = arrival.Add(5 * time.Minute)
		h.scan(h.cfg)
		if got := h.rings(); got != 1 {
			t.Fatalf("rings at the addition's hold = %d, want 1", got)
		}
	})
}
