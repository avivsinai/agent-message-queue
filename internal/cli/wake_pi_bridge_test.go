package cli

// The wake side of the pi bridge doorbell (bead agent-message-queue-e1gx).
// The rows are the rulings after the live rows on real pi: one ask per wake
// tick (D1); the busy bound starts at the cohort's first refusal and equals
// input_max_hold, and the fallback line says why (D2); the exclusive create
// of "withdrawn" is the only authority to type (D3); a decision is never
// deleted or rewritten (D4); acceptance is evidence that arrives later, and
// the ledger records it when the wake sees the result on a later tick (D5);
// the request names the pi process it is for (D6); retained attempts are
// bounded without a slot table (D7).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/notificationattempt"
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
	// bridgeDecision is what the simulated bridge writes for each newly
	// published request, during the wake's decision poll: "claimed",
	// "refused", "garbage" (an unreadable decision file), or "" (no answer).
	bridgeDecision string
	sleepCount     int
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
			h.sleepCount++
			h.respond()
		},
	}
	return h
}

// respond is the simulated bridge: during the wake's decision poll it
// writes the configured decision for the newest undecided request, exactly
// as the extension would.
func (h *wakePiBridgeHarness) respond() {
	h.t.Helper()
	if h.live == nil || h.bridgeDecision == "" {
		return
	}
	dir := filepath.Join(h.root, "agents", "codex", "extensions", "pi-bridge", "doorbell")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	attemptID := ""
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".decision.json") || strings.HasSuffix(name, ".result.json") {
			continue
		}
		candidate := strings.TrimSuffix(name, ".json")
		if _, err := os.Stat(filepath.Join(dir, candidate+".decision.json")); err == nil {
			continue
		}
		attemptID = candidate
		break
	}
	if attemptID == "" {
		return
	}
	if h.bridgeDecision == "garbage" {
		if err := os.WriteFile(filepath.Join(dir, attemptID+".decision.json"), []byte("{not json"), 0o600); err != nil {
			h.t.Fatal(err)
		}
		return
	}
	payload, err := json.Marshal(wakePiBridgeDecision{
		Protocol: wakePiBridgeProtocol,
		Attempt:  attemptID,
		Decision: h.bridgeDecision,
		Reason:   "pi is busy with other work",
	})
	if err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, attemptID+".decision.json"), payload, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *wakePiBridgeHarness) capableBridge() {
	h.live = &wakePiBridgeLiveness{PID: 4242, Generation: "gen-1", Surface: "tui", Doorbell: true}
}

func (h *wakePiBridgeHarness) deliverPending() {
	h.t.Helper()
	files := wakeDoorbellTestFiles(h.t, "pending.md")
	h.cfg.doorbell.arm(files)
}

// publishedRequests lists the request files under doorbell/.
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
		if strings.HasSuffix(e.Name(), ".json") && !strings.HasSuffix(e.Name(), ".decision.json") && !strings.HasSuffix(e.Name(), ".result.json") {
			names = append(names, e.Name())
		}
	}
	return names
}

// doorbellDirFileCount counts every file under doorbell/.
func (h *wakePiBridgeHarness) doorbellDirFileCount() int {
	h.t.Helper()
	entries, err := os.ReadDir(filepath.Join(h.root, "agents", "codex", "extensions", "pi-bridge", "doorbell"))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return len(entries)
}

// ledgerLog reads the notification attempt journal, "" when there is none.
func (h *wakePiBridgeHarness) ledgerLog() string {
	h.t.Helper()
	data, err := os.ReadFile(filepath.Join(h.root, "agents", "codex", "receipts", notificationattempt.LogFilename))
	if err != nil {
		return ""
	}
	return string(data)
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

func (h *wakePiBridgeHarness) fullLineTyped() bool {
	got := h.typedChunks()
	return len(got) == 2 && strings.Contains(got[0], coopWakeDoorbell)
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
			if !h.fullLineTyped() {
				t.Fatal("terminal delivery did not run the full fixed line")
			}
		})
	}
}

// TestWakePiBridgeClaimPresentsWithoutSubmitOnly: a claimed attempt types
// nothing, records the presentation on the cohort's cadence (announced
// phase, a retry deadline), and leaves presentationConfirmed false, so the
// next reminder plans the full fixed line, never a bare Enter. The request
// names the pi process it is for, not the wake's pid.
func TestWakePiBridgeClaimPresentsWithoutSubmitOnly(t *testing.T) {
	h := newWakePiBridgeHarness(t)
	h.capableBridge()
	h.bridgeDecision = "claimed"
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
	dir := filepath.Join(h.root, "agents", "codex", "extensions", "pi-bridge", "doorbell")
	names := h.publishedRequests()
	if len(names) != 1 {
		t.Fatalf("published requests = %v, want one", names)
	}
	data, err := os.ReadFile(filepath.Join(dir, names[0]))
	if err != nil {
		t.Fatal(err)
	}
	var req wakePiBridgeRequest
	if err := json.Unmarshal(data, &req); err != nil {
		t.Fatal(err)
	}
	if req.PID != 4242 {
		t.Fatalf("request pid = %d, want the bridge's 4242", req.PID)
	}
}

// TestWakePiBridgeBusyBoundHoldsAcrossTicksThenFallsBack is the live-row
// regression: one ask per tick, so a busy seat types nothing while the
// input_max_hold bound (measured from the cohort's first refusal) is open,
// however many ticks it takes; past the bound the terminal path runs and
// its first write is the full fixed line.
func TestWakePiBridgeBusyBoundHoldsAcrossTicksThenFallsBack(t *testing.T) {
	h := newWakePiBridgeHarness(t)
	h.capableBridge()
	h.cfg.settings.inputMaxHold = 15 * time.Second
	h.bridgeDecision = "refused"
	h.deliverPending()
	for i := 0; i < 8; i++ {
		h.deliver()
		if got := h.typedChunks(); len(got) != 0 {
			t.Fatalf("a busy hold typed at tick %d: %q", i, got)
		}
		h.advance(2 * time.Second)
	}
	// 16s after the first refusal the bound is over: the fallback runs.
	h.deliver()
	if !h.fullLineTyped() {
		t.Fatal("the fallback did not write the full fixed line once the busy bound passed")
	}
}

// TestWakePiBridgeZeroHoldFallsBackAtOnce: with input_max_hold 0 one busy
// refusal is enough for the immediate terminal fallback.
func TestWakePiBridgeZeroHoldFallsBackAtOnce(t *testing.T) {
	h := newWakePiBridgeHarness(t)
	h.capableBridge()
	h.cfg.settings.inputMaxHold = 0
	h.bridgeDecision = "refused"
	h.deliverPending()
	h.deliver()

	if !h.fullLineTyped() {
		t.Fatal("a zero hold did not fall back at once")
	}
	for _, d := range h.sleeps {
		if d > wakePiBridgePollInterval {
			t.Fatalf("a zero hold slept %v beyond the poll interval", d)
		}
	}
}

// TestWakePiBridgeDeadlineWithdrawsAndFallsBack: with no decision in time
// the wake's exclusive create of withdrawn succeeds, and the terminal path
// types the full fixed line.
func TestWakePiBridgeDeadlineWithdrawsAndFallsBack(t *testing.T) {
	h := newWakePiBridgeHarness(t)
	h.capableBridge()
	h.deliverPending()
	h.deliver()

	if !h.fullLineTyped() {
		t.Fatal("fallback wrote something other than the full fixed line then the submit")
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

// TestWakePiBridgeUnreadableDecisionTypesNothing: when the withdrawal
// create fails because a decision exists and that decision is unreadable,
// no winner is proven, so the wake types nothing this pass and never
// rewrites the decision.
func TestWakePiBridgeUnreadableDecisionTypesNothing(t *testing.T) {
	h := newWakePiBridgeHarness(t)
	h.capableBridge()
	h.bridgeDecision = "garbage"
	h.deliverPending()
	h.deliver()

	if got := h.typedChunks(); len(got) != 0 {
		t.Fatalf("an unreadable decision typed into the terminal: %q", got)
	}
	dir := filepath.Join(h.root, "agents", "codex", "extensions", "pi-bridge", "doorbell")
	names := h.publishedRequests()
	if len(names) != 1 {
		t.Fatalf("published requests = %v, want one", names)
	}
	decision, err := os.ReadFile(filepath.Join(dir, strings.TrimSuffix(names[0], ".json")+".decision.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(decision) != "{not json" {
		t.Fatalf("the decision file was rewritten: %q", decision)
	}
}

// TestWakePiBridgeClaimReadBackAfterDeadlinePresents: the bridge writes its
// claim in the window between the wake's deadline and the withdrawal
// create; the existing decision decides, the create fails, the re-read
// finds the claim, the claim presents, and nothing is typed.
func TestWakePiBridgeClaimReadBackAfterDeadlinePresents(t *testing.T) {
	h := newWakePiBridgeHarness(t)
	h.capableBridge()
	realCreate := piBridgeWithdrawCreate
	t.Cleanup(func() { piBridgeWithdrawCreate = realCreate })
	piBridgeWithdrawCreate = func(path string, data []byte, perm os.FileMode) (bool, error) {
		// The bridge's late claim lands first; the wake's create then
		// loses to the existing decision.
		attemptID := strings.TrimSuffix(filepath.Base(path), ".decision.json")
		claim, err := json.Marshal(wakePiBridgeDecision{Protocol: wakePiBridgeProtocol, Attempt: attemptID, Decision: "claimed"})
		if err != nil {
			return false, err
		}
		if err := os.WriteFile(path, claim, perm); err != nil {
			return false, err
		}
		return realCreate(path, data, perm)
	}
	h.deliverPending()
	h.deliver()

	if got := h.typedChunks(); len(got) != 0 {
		t.Fatalf("the claim typed into the terminal: %q", got)
	}
	if h.cfg.doorbell.phase != wakeDoorbellAnnounced {
		t.Fatalf("phase = %v, want announced (presented)", h.cfg.doorbell.phase)
	}
	dir := filepath.Join(h.root, "agents", "codex", "extensions", "pi-bridge", "doorbell")
	decision, err := os.ReadFile(filepath.Join(dir, strings.TrimSuffix(h.publishedRequests()[0], ".json")+".decision.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(decision) == "" || !strings.Contains(string(decision), "claimed") {
		t.Fatalf("the wake rewrote the bridge's decision: %q", decision)
	}
}

// TestWakePiBridgeClaimThenLaterReminderFallsBackWithFullLine: a bridge
// claim presents the cohort; on a later reminder the bridge is gone, and
// the fallback write is the full fixed line, not a bare Enter — the claim
// alone never authorized submit-only.
func TestWakePiBridgeClaimThenLaterReminderFallsBackWithFullLine(t *testing.T) {
	h := newWakePiBridgeHarness(t)
	h.capableBridge()
	h.bridgeDecision = "claimed"
	h.deliverPending()
	h.deliver()
	if got := h.typedChunks(); len(got) != 0 {
		t.Fatalf("the claim typed into the terminal: %q", got)
	}

	// Later reminder: the bridge is gone. The cohort's retry ladder still
	// plans an attempt; the delivery falls back to the terminal with the
	// full line.
	h.live = nil
	h.bridgeDecision = ""
	h.advance(10 * time.Minute)
	h.deliver()
	if !h.fullLineTyped() {
		t.Fatal("fallback wrote something other than the full fixed line then the submit")
	}
}

// TestWakePiBridgeAcceptanceRecordsOnLaterTick: the claim's ledger record
// says presented, never accepted; when the acceptance result arrives, the
// wake records it in the attempt ledger on a later tick.
func TestWakePiBridgeAcceptanceRecordsOnLaterTick(t *testing.T) {
	h := newWakePiBridgeHarness(t)
	h.capableBridge()
	h.bridgeDecision = "claimed"
	h.deliverPending()
	h.deliver()

	if got := h.typedChunks(); len(got) != 0 {
		t.Fatalf("the claim typed into the terminal: %q", got)
	}
	log := h.ledgerLog()
	if !strings.Contains(log, "presented by the pi bridge") {
		t.Fatalf("the ledger lost the presented record: %s", log)
	}
	if strings.Contains(log, "accepted") {
		t.Fatalf("a claim was labeled accepted at claim time: %s", log)
	}

	// The bridge publishes the acceptance result; the next tick records it.
	h.advance(10 * time.Minute)
	attemptID := strings.TrimSuffix(h.publishedRequests()[0], ".json")
	dir := filepath.Join(h.root, "agents", "codex", "extensions", "pi-bridge", "doorbell")
	result := map[string]any{
		"protocol": wakePiBridgeProtocol,
		"accepted": true,
		"attempt":  attemptID,
		"at":       time.Now().UTC().Format(time.RFC3339),
	}
	payload, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, attemptID+".result.json"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	h.deliver()
	log = h.ledgerLog()
	if !strings.Contains(log, "accepted by pi (message_start)") {
		t.Fatalf("the ledger lost the acceptance record: %s", log)
	}
}

// TestWakePiBridgeRefusedAttemptsArePruned: the wake removes its own
// finished refused attempts as it asks again, so the retained directory
// stays at the current attempt plus what may still produce evidence.
func TestWakePiBridgeRefusedAttemptsArePruned(t *testing.T) {
	h := newWakePiBridgeHarness(t)
	h.capableBridge()
	h.bridgeDecision = "refused"
	h.deliverPending()
	for i := 0; i < 3; i++ {
		h.deliver()
		h.advance(2 * time.Second)
	}
	if got := h.doorbellDirFileCount(); got != 2 {
		t.Fatalf("doorbell dir holds %d files after refused asks, want the current attempt's two", got)
	}
	h.bridgeDecision = "claimed"
	h.deliver()
	if got := h.doorbellDirFileCount(); got != 2 {
		t.Fatalf("doorbell dir holds %d files after the claim, want the claimed attempt's two", got)
	}
	if names := h.publishedRequests(); len(names) != 1 {
		t.Fatalf("published requests = %v, want one", names)
	}
}
