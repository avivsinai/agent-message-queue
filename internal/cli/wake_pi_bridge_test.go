package cli

// The wake side of the pi bridge doorbell (bead agent-message-queue-e1gx).
// The rows are the ratified P1–P4 decisions: a capable bridge takes the
// attempt with no terminal write; a claim counts as a presented doorbell
// for the cohort's cadence and caps but never sets the submit-only state;
// a busy seat types nothing until the input_max_hold bound, then the
// terminal path runs with a one-line fallback; the wake's deadline
// withdraws the attempt; and a bridge claim followed by a terminal
// fallback types the full fixed line, never a bare Enter.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type wakePiBridgeHarness struct {
	t        *testing.T
	root     string
	now      time.Time
	cfg      *wakeConfig
	injected []string
	sleeps   []time.Duration
	proofErr error
	live     *wakePiBridgeLiveness
}

func newWakePiBridgeHarness(t *testing.T) *wakePiBridgeHarness {
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
	h := &wakePiBridgeHarness{t: t, root: root, now: time.Unix(1_800_000_000, 0)}
	h.cfg = &wakeConfig{
		me:             "codex",
		root:           root,
		session:        "s",
		wakeOwner:      &wakeOwner{PID: 4242},
		injectMode:     wakeInjectModePaste,
		retryUntil:     wakeRetryUntilInjected,
		settings:       wakeSettings{},
		cohortIDs:      []string{"m1"},
		doorbellNow:    func() time.Time { return h.now },
		attentionIsTTY: func() bool { return false },
		terminalWrite: func(text string) error {
			h.injected = append(h.injected, text)
			return nil
		},
		recordNotifierStatus: func(string, string, string) error { return nil },
		piBridgeLivenessRead: func(time.Time) (wakePiBridgeLiveness, bool) {
			if h.live == nil {
				return wakePiBridgeLiveness{}, false
			}
			return *h.live, true
		},
		piBridgeOwnerProof: func(int) error { return h.proofErr },
		piBridgeSleep: func(d time.Duration) {
			h.sleeps = append(h.sleeps, d)
			h.now = h.now.Add(d)
		},
	}
	return h
}

func (h *wakePiBridgeHarness) capableBridge() {
	h.live = &wakePiBridgeLiveness{PID: 4242, Generation: "gen-1", Surface: "tui", Doorbell: true}
}

func (h *wakePiBridgeHarness) deliverPending() {
	h.t.Helper()
	files := wakeDoorbellTestFiles(h.t, "pending.md")
	h.cfg.doorbell.arm(files)
}

// decide pre-places the bridge's decision for one attempt, as the extension
// would have written it before the wake's poll reads it.
func (h *wakePiBridgeHarness) decide(seq int, decision, reason string) {
	h.t.Helper()
	dir := filepath.Join(h.root, "agents", "codex", "extensions", "pi-bridge", "doorbell")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		h.t.Fatal(err)
	}
	attemptID := wakePiBridgeAttemptID(h.cfg.cohortIDs, seq)
	payload, err := json.Marshal(wakePiBridgeDecision{
		Protocol: wakePiBridgeProtocol,
		Attempt:  attemptID,
		Decision: decision,
		Reason:   reason,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, attemptID+".decision.json"), payload, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// publishedRequests lists the request files the wake wrote.
func (h *wakePiBridgeHarness) publishedRequests() []string {
	h.t.Helper()
	entries, err := os.ReadDir(filepath.Join(h.root, "agents", "codex", "extensions", "pi-bridge", "doorbell"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		h.t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") && !strings.HasSuffix(e.Name(), ".decision.json") {
			names = append(names, e.Name())
		}
	}
	return names
}

// typedChunks returns the terminal chunks typed and clears the record.
func (h *wakePiBridgeHarness) typedChunks() []string {
	got := h.injected
	h.injected = nil
	return got
}

func (h *wakePiBridgeHarness) advance(d time.Duration) { h.now = h.now.Add(d) }

func (h *wakePiBridgeHarness) deliver() {
	h.t.Helper()
	current := wakeDoorbellTestFiles(h.t, "pending.md")
	notice := peerWakeNotification("test message")
	if err := deliverNewMessageNotification(h.cfg, notice, false, current); err != nil {
		h.t.Fatalf("deliverNewMessageNotification: %v", err)
	}
}

// TestWakePiBridgeNoCapableBridgeTakesTodaysPath: no fresh live capable
// record (absent, wrong surface, no doorbell capability) or an owner proof
// failure leaves the delivery to the terminal exactly as before, and the
// wake publishes nothing under doorbell/.
func TestWakePiBridgeNoCapableBridgeTakesTodaysPath(t *testing.T) {
	for name, setup := range map[string]func(*wakePiBridgeHarness){
		"absent": func(h *wakePiBridgeHarness) {},
		"other surface": func(h *wakePiBridgeHarness) {
			h.live = &wakePiBridgeLiveness{PID: 4242, Generation: "gen-1", Surface: "rpc", Doorbell: true}
		},
		"no capability": func(h *wakePiBridgeHarness) {
			h.live = &wakePiBridgeLiveness{PID: 4242, Generation: "gen-1", Surface: "tui"}
		},
		"owner proof": func(h *wakePiBridgeHarness) { h.capableBridge(); h.proofErr = os.ErrPermission },
	} {
		t.Run(name, func(t *testing.T) {
			h := newWakePiBridgeHarness(t)
			setup(h)
			h.deliverPending()
			h.deliver()
			if names := h.publishedRequests(); len(names) != 0 {
				t.Fatalf("bridge requests published: %v", names)
			}
			if got := h.typedChunks(); len(got) == 0 {
				t.Fatal("terminal delivery did not run")
			}
		})
	}
}

// TestWakePiBridgeClaimPresentsWithoutSubmitOnly: a claimed attempt types
// nothing, records the presentation on the cohort's cadence (announced
// phase, a retry deadline), and leaves presentationConfirmed false, so the
// next reminder plans the full fixed line, never a bare Enter.
func TestWakePiBridgeClaimPresentsWithoutSubmitOnly(t *testing.T) {
	h := newWakePiBridgeHarness(t)
	h.capableBridge()
	h.decide(1, "claimed", "")
	h.deliverPending()
	h.deliver()

	if got := h.typedChunks(); len(got) != 0 {
		t.Fatalf("a claim typed into the terminal: %q", got)
	}
	if h.cfg.doorbell.presentationConfirmed {
		t.Fatal("a bridge claim set the submit-only state")
	}
	if h.cfg.doorbell.phase != wakeDoorbellAnnounced {
		t.Fatalf("phase = %v, want announced (presented)", h.cfg.doorbell.phase)
	}
}

// TestWakePiBridgeBusyHoldsThenFallsBackWithFullLine: busy refusals type
// nothing while the input_max_hold bound is open and retry; past the bound
// the terminal path runs and its first write is the full fixed line.
func TestWakePiBridgeBusyHoldsThenFallsBackWithFullLine(t *testing.T) {
	h := newWakePiBridgeHarness(t)
	h.capableBridge()
	h.cfg.settings.inputMaxHold = 5 * time.Second
	for seq := 1; seq <= 6; seq++ {
		h.decide(seq, "refused", "pi is busy")
	}
	h.deliverPending()
	h.deliver()

	if len(h.sleeps) == 0 {
		t.Fatal("a busy seat did not hold before the fallback")
	}
	got := h.typedChunks()
	if len(got) != 2 || !strings.Contains(got[0], coopWakeDoorbell) {
		t.Fatalf("fallback wrote %q, want the full fixed line then the submit", got)
	}
}

// TestWakePiBridgeZeroHoldFallsBackAtOnce: with input_max_hold 0 one busy
// refusal is enough for the immediate terminal fallback.
func TestWakePiBridgeZeroHoldFallsBackAtOnce(t *testing.T) {
	h := newWakePiBridgeHarness(t)
	h.capableBridge()
	h.cfg.settings.inputMaxHold = 0
	h.decide(1, "refused", "pi is busy")
	h.deliverPending()
	h.deliver()

	got := h.typedChunks()
	if len(got) != 2 || !strings.Contains(got[0], coopWakeDoorbell) {
		t.Fatalf("fallback wrote %q, want the full fixed line then the submit", got)
	}
	if len(h.sleeps) != 0 {
		t.Fatalf("a zero hold slept %v", h.sleeps)
	}
}

// TestWakePiBridgeDeadlineWithdrawsAndFallsBack: with no decision in time
// the wake withdraws the attempt, and the terminal path types the full
// fixed line.
func TestWakePiBridgeDeadlineWithdrawsAndFallsBack(t *testing.T) {
	h := newWakePiBridgeHarness(t)
	h.capableBridge()
	h.deliverPending()
	h.deliver()

	got := h.typedChunks()
	if len(got) != 2 || !strings.Contains(got[0], coopWakeDoorbell) {
		t.Fatalf("fallback wrote %q, want the full fixed line then the submit", got)
	}
	dir := filepath.Join(h.root, "agents", "codex", "extensions", "pi-bridge", "doorbell")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var withdrawn int
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".decision.json") {
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			var dec wakePiBridgeDecision
			if err := json.Unmarshal(data, &dec); err != nil {
				t.Fatal(err)
			}
			if dec.Decision != "withdrawn" {
				t.Fatalf("decision %q, want withdrawn", dec.Decision)
			}
			withdrawn++
		}
	}
	if withdrawn == 0 {
		t.Fatal("the wake did not withdraw its attempt")
	}
}

// TestWakePiBridgeClaimThenLaterReminderFallsBackWithFullLine is the
// ratified addition row: a bridge claim presents the cohort; on a later
// reminder the bridge is gone, and the fallback write is the full fixed
// line, not a bare Enter — the claim alone never authorized submit-only.
func TestWakePiBridgeClaimThenLaterReminderFallsBackWithFullLine(t *testing.T) {
	h := newWakePiBridgeHarness(t)
	h.capableBridge()
	h.decide(1, "claimed", "")
	h.deliverPending()
	h.deliver()
	if got := h.typedChunks(); len(got) != 0 {
		t.Fatalf("the claim typed into the terminal: %q", got)
	}

	// Later reminder: the bridge is gone. The cohort's retry ladder still
	// plans an attempt; the delivery falls back to the terminal with the
	// full line.
	h.live = nil
	h.advance(10 * time.Minute)
	h.deliver()
	got := h.typedChunks()
	if len(got) != 2 || !strings.Contains(got[0], coopWakeDoorbell) {
		t.Fatalf("fallback wrote %q, want the full fixed line then the submit", got)
	}
}
