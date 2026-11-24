package cli

// The wake side of the pi bridge doorbell (bead agent-message-queue-e1gx).
// The rows are the rulings after the live rows on real pi: one ask per wake
// tick, and the ask never waits — a pending ask finishes on a later tick
// (D1); the busy bound starts at the cohort's first refusal and equals
// input_max_hold, and the fallback line says why (D2); the exclusive create
// of "withdrawn" is the only authority to type (D3); a decision is never
// deleted or rewritten (D4); acceptance is evidence that arrives later,
// collected on ordinary ticks and attributed to its own claim's cohort,
// marked seen only after the record lands (D5); the request names the pi
// process it is for (D6); only proven-finished attempts are retired, and
// the retention cap never evicts an outstanding group (D7).

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
	proofErr error
	// pending is one stable pending-file identity for the whole harness:
	// a real inbox file keeps its identity between scans, and a fresh
	// fabricated mtime per deliver would look like an in-place
	// replacement and reset the ladder under the collector's accounting.
	pending map[string]os.FileInfo
	live    *wakePiBridgeLiveness
	// bridgeDecision is what the simulated bridge writes when respond()
	// runs: "claimed", "refused", "garbage" (an unreadable decision file),
	// or "" (no answer).
	bridgeDecision string
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
	}
	return h
}

// respond is the simulated bridge: it writes the configured decision for
// the newest undecided request, exactly as the extension would between two
// wake ticks. The wake itself never waits, so the test drives it.
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
		if !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".decision.json") ||
			strings.HasSuffix(name, ".result.json") || strings.HasSuffix(name, ".cohort.json") ||
			strings.HasSuffix(name, ".turn.json") {
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

func (h *wakePiBridgeHarness) pendingFiles() map[string]os.FileInfo {
	h.t.Helper()
	if h.pending == nil {
		h.pending = wakeDoorbellTestFiles(h.t, "pending.md")
	}
	return h.pending
}

func (h *wakePiBridgeHarness) deliverPending() {
	h.t.Helper()
	h.cfg.doorbell.arm(h.pendingFiles())
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
		if strings.HasSuffix(e.Name(), ".json") && !strings.HasSuffix(e.Name(), ".decision.json") &&
			!strings.HasSuffix(e.Name(), ".result.json") && !strings.HasSuffix(e.Name(), ".cohort.json") &&
			!strings.HasSuffix(e.Name(), ".turn.json") {
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

// ledgerDetailsFor returns the ledger result rows recorded under one
// message id, so a test can check each message's own ledger.
func (h *wakePiBridgeHarness) ledgerDetailsFor(messageID string) []string {
	h.t.Helper()
	log := h.ledgerLog()
	if log == "" {
		return nil
	}
	var details []string
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		var rec notificationattempt.Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec.Phase != "result" {
			continue
		}
		for _, id := range rec.MessageIDs {
			if id == messageID {
				details = append(details, rec.Detail)
				break
			}
		}
	}
	return details
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
	current := h.pendingFiles()
	notice := peerWakeNotification("test message")
	if err := deliverNewMessageNotification(h.cfg, notice, false, current); err != nil {
		h.t.Fatalf("deliverNewMessageNotification: %v", err)
	}
}

// presentViaBridge walks one full bridge presentation: the publish ask,
// the bridge's decision, and the tick that finishes the ask.
func (h *wakePiBridgeHarness) presentViaBridge() {
	h.t.Helper()
	h.deliver()
	h.respond()
	h.deliver()
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
// names the pi process it is for, not the wake's pid. The ask itself never
// waits: the publish ask returns engaged, and the next tick finishes it.
func TestWakePiBridgeClaimPresentsWithoutSubmitOnly(t *testing.T) {
	h := newWakePiBridgeHarness(t)
	h.capableBridge()
	h.bridgeDecision = "claimed"
	h.deliverPending()
	h.presentViaBridge()

	if got := h.typedChunks(); len(got) != 0 {
		t.Fatalf("a claim typed into the terminal: %q", got)
	}
	if h.cfg.doorbell.presentationConfirmed {
		t.Fatal("a bridge claim set the submit-only state")
	}
	// The presentation advances the cohort's ladder and budget exactly as a
	// typed attempt does: phase retrying, next attempt on the 5s base.
	if h.cfg.doorbell.phase != wakeDoorbellRetrying {
		t.Fatalf("phase = %v, want retrying (presented on the ladder)", h.cfg.doorbell.phase)
	}
	if h.cfg.doorbell.attempts != 1 {
		t.Fatalf("attempts = %d, want the presentation to advance the cadence once", h.cfg.doorbell.attempts)
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
	h.deliver() // the publish ask; the refusal lands between ticks
	h.respond()
	for i := 0; i < 8; i++ {
		h.deliver()
		if got := h.typedChunks(); len(got) != 0 {
			t.Fatalf("a busy hold typed at tick %d: %q", i, got)
		}
		h.respond()
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
	h.respond()
	h.deliver()

	if !h.fullLineTyped() {
		t.Fatal("a zero hold did not fall back at once")
	}
}

// TestWakePiBridgeDeadlineWithdrawsAndFallsBack: with no decision by the
// request deadline, the wake's exclusive create of withdrawn succeeds, and
// the terminal path types the full fixed line.
func TestWakePiBridgeDeadlineWithdrawsAndFallsBack(t *testing.T) {
	h := newWakePiBridgeHarness(t)
	h.capableBridge()
	h.deliverPending()
	h.deliver() // the publish ask
	h.advance(3 * time.Second)
	h.deliver() // the deadline tick: the withdrawn create is the authority

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

// TestWakePiBridgeUnreadableDecisionTypesNothing: a decision file that
// exists but is unreadable proves no winner, so the wake types nothing
// this pass and never rewrites the decision.
func TestWakePiBridgeUnreadableDecisionTypesNothing(t *testing.T) {
	h := newWakePiBridgeHarness(t)
	h.capableBridge()
	h.bridgeDecision = "garbage"
	h.deliverPending()
	h.deliver()
	h.respond()
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
	h.advance(3 * time.Second)
	h.deliver()

	if got := h.typedChunks(); len(got) != 0 {
		t.Fatalf("the claim typed into the terminal: %q", got)
	}
	// The presentation advances the cohort's ladder and budget exactly as a
	// typed attempt does: phase retrying, next attempt on the 5s base.
	if h.cfg.doorbell.phase != wakeDoorbellRetrying {
		t.Fatalf("phase = %v, want retrying (presented on the ladder)", h.cfg.doorbell.phase)
	}
	if h.cfg.doorbell.attempts != 1 {
		t.Fatalf("attempts = %d, want the presentation to advance the cadence once", h.cfg.doorbell.attempts)
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
	h.presentViaBridge()
	if got := h.typedChunks(); len(got) != 0 {
		t.Fatalf("the claim typed into the terminal: %q", got)
	}

	// Later reminder: the bridge is gone. The unresolved claim is not
	// evidence of non-delivery, so this pass retires it and holds; the
	// next pass finds no claim and no fresh bridge, and the terminal
	// fallback owns the full line.
	h.live = nil
	h.bridgeDecision = ""
	h.advance(10*time.Minute + 5*time.Second)
	h.deliver()
	if got := h.typedChunks(); len(got) != 0 {
		t.Fatalf("the lost claim typed into the terminal itself: %q", got)
	}
	h.advance(11 * time.Second)
	h.deliver()
	if !h.fullLineTyped() {
		t.Fatalf("fallback wrote something other than the full fixed line then the submit: %q", h.typedChunks())
	}
}

// TestWakePiBridgeAcceptanceRecordsOnEmptyInboxTick: the claim's ledger
// record says presented, never accepted; the acceptance result is recorded
// on an ordinary wake tick with an empty inbox, with no delivery at all.
func TestWakePiBridgeAcceptanceRecordsOnEmptyInboxTick(t *testing.T) {
	h := newWakePiBridgeHarness(t)
	h.capableBridge()
	h.bridgeDecision = "claimed"
	h.deliverPending()
	h.presentViaBridge()

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

	// The bridge publishes the acceptance result; the next ordinary tick
	// (maintain) records it with no mail and no delivery in between.
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
	h.cfg.collectPiBridgeResults()
	log = h.ledgerLog()
	if !strings.Contains(log, "accepted by pi (message_start)") {
		t.Fatalf("the ledger lost the acceptance record: %s", log)
	}
}

// TestWakePiBridgeAcceptanceFollowsItsOwnCohort is the live-run regression:
// message A presents through the bridge; its acceptance result arrives; the
// next delivery is for a different message B. The acceptance is recorded
// under A's own ledger and never under B's, whatever B's delivery carries.
func TestWakePiBridgeAcceptanceFollowsItsOwnCohort(t *testing.T) {
	h := newWakePiBridgeHarness(t)
	h.capableBridge()
	h.bridgeDecision = "claimed"
	h.deliverPending()
	h.presentViaBridge()
	attemptA := strings.TrimSuffix(h.publishedRequests()[0], ".json")

	// The bridge publishes A's acceptance result. No collection has run
	// yet; the next delivery is for a different message.
	dir := filepath.Join(h.root, "agents", "codex", "extensions", "pi-bridge", "doorbell")
	result := map[string]any{
		"protocol": wakePiBridgeProtocol,
		"accepted": true,
		"attempt":  attemptA,
		"at":       time.Now().UTC().Format(time.RFC3339),
	}
	payload, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, attemptA+".result.json"), payload, 0o600); err != nil {
		t.Fatal(err)
	}

	// Message B arrives: a new cohort, a new attempt.
	h.cfg.cohortIDs = []string{"m2"}
	files := wakeDoorbellTestFiles(h.t, "pending-b.md")
	h.cfg.doorbell.arm(files)
	h.presentViaBridge()
	attemptB := ""
	for _, name := range h.publishedRequests() {
		if id := strings.TrimSuffix(name, ".json"); id != attemptA {
			attemptB = id
		}
	}

	// A's ledger holds the presentation and the acceptance; B's ledger
	// holds nothing of A's attempt.
	aDetails := h.ledgerDetailsFor("m1")
	if len(aDetails) == 0 || !strings.Contains(strings.Join(aDetails, "\n"), "accepted by pi (message_start)") {
		t.Fatalf("A's ledger lost the acceptance: %q", aDetails)
	}
	for _, detail := range aDetails {
		if !strings.Contains(detail, attemptA) {
			t.Fatalf("A's ledger names another attempt: %q", detail)
		}
	}
	for _, detail := range h.ledgerDetailsFor("m2") {
		if strings.Contains(detail, attemptA) {
			t.Fatalf("B's ledger recorded A's acceptance: %q", detail)
		}
	}
	if !strings.Contains(strings.Join(h.ledgerDetailsFor("m2"), "\n"), attemptB) {
		t.Fatal("B's ledger lost its own presentation")
	}
}

// TestWakePiBridgeRefusedAttemptsArePruned: the wake retires its own
// proven-finished refused attempts, so the retained directory holds only
// the current ask.
func TestWakePiBridgeRefusedAttemptsArePruned(t *testing.T) {
	h := newWakePiBridgeHarness(t)
	h.capableBridge()
	h.cfg.settings.inputMaxHold = 15 * time.Second
	h.bridgeDecision = "refused"
	h.deliverPending()
	h.deliver() // publish a1
	h.respond()
	h.advance(2 * time.Second)
	h.deliver() // read the refusal
	h.advance(2 * time.Second)
	h.deliver() // retire a1, publish a2
	// A published attempt is now request + cohort record: the cohort is
	// written at publish time so a result can never miss it.
	if got := h.doorbellDirFileCount(); got != 2 {
		t.Fatalf("doorbell dir holds %d files after the retired refusal, want a2's request plus its cohort record", got)
	}
	h.respond()
	h.advance(2 * time.Second)
	h.deliver() // read the refusal
	h.advance(2 * time.Second)
	h.deliver() // retire a2, publish a3
	if names := h.publishedRequests(); len(names) != 1 {
		t.Fatalf("published requests = %v, want one", names)
	}
	h.bridgeDecision = "claimed"
	h.respond()
	h.advance(2 * time.Second)
	h.deliver() // read the claim
	if got := h.doorbellDirFileCount(); got != 3 {
		t.Fatalf("doorbell dir holds %d files after the claim, want the claimed attempt's three (request, decision, cohort)", got)
	}
	if names := h.publishedRequests(); len(names) != 1 {
		t.Fatalf("published requests = %v, want one", names)
	}
}

// TestWakePiBridgePruneKeepsOutstandingGroups: only proven-finished groups
// are retired — a readable refused/failed/withdrawn decision, or a claim
// whose result the ledger recorded. An undecided request, an unreadable
// decision, and a claim whose result has not arrived stay, whatever their
// deadline says.
func TestWakePiBridgePruneKeepsOutstandingGroups(t *testing.T) {
	h := newWakePiBridgeHarness(t)
	dir := filepath.Join(h.root, "agents", "codex", "extensions", "pi-bridge", "doorbell")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	past := h.now.Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	writeGroup := func(t *testing.T, id string, decision string, rawDecision []byte, withResult bool) {
		t.Helper()
		req, err := json.Marshal(wakePiBridgeRequest{Protocol: wakePiBridgeProtocol, Op: "doorbell", Attempt: id, Deadline: past})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, id+".json"), req, 0o600); err != nil {
			t.Fatal(err)
		}
		switch {
		case decision != "":
			payload, err := json.Marshal(wakePiBridgeDecision{Protocol: wakePiBridgeProtocol, Attempt: id, Decision: decision})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, id+".decision.json"), payload, 0o600); err != nil {
				t.Fatal(err)
			}
		case rawDecision != nil:
			if err := os.WriteFile(filepath.Join(dir, id+".decision.json"), rawDecision, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if decision == "claimed" {
			cohort, err := json.Marshal(wakePiBridgeCohort{Protocol: wakePiBridgeProtocol, Attempt: id, MessageIDs: []string{"m1"}})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, id+".cohort.json"), cohort, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if withResult {
			payload, err := json.Marshal(map[string]any{"protocol": wakePiBridgeProtocol, "accepted": true, "attempt": id})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, id+".result.json"), payload, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeGroup(t, "doorbell-withdrawn", "withdrawn", nil, false)
	writeGroup(t, "doorbell-refused", "refused", nil, false)
	writeGroup(t, "doorbell-garbage", "", []byte("{not json"), false)
	writeGroup(t, "doorbell-undecided", "", nil, false)
	writeGroup(t, "doorbell-claim-open", "claimed", nil, false)
	writeGroup(t, "doorbell-claim-done", "claimed", nil, true)

	seen := map[string]bool{"doorbell-claim-done.result.json": true}
	processed := map[string]bool{"doorbell-withdrawn": true, "doorbell-refused": true, "doorbell-claim-done": true}
	pruneWakePiBridgeAttempts(dir, seen, processed, time.Now())

	for _, kept := range []string{"doorbell-garbage", "doorbell-undecided", "doorbell-claim-open"} {
		if _, err := os.Stat(filepath.Join(dir, kept+".json")); err != nil {
			t.Fatalf("an outstanding group was retired: %s", kept)
		}
	}
	for _, gone := range []string{"doorbell-withdrawn", "doorbell-refused", "doorbell-claim-done"} {
		if _, err := os.Stat(filepath.Join(dir, gone+".json")); err == nil {
			t.Fatalf("a proven-finished group was kept: %s", gone)
		}
	}
	if seen["doorbell-claim-done.result.json"] {
		t.Fatal("the tracking map kept an entry whose group was retired")
	}
}

// deliverEmpty runs one delivery pass with an empty inbox: the drain case,
// where the mail is gone before the wake's next tick.
func (h *wakePiBridgeHarness) deliverEmpty() {
	h.t.Helper()
	if err := deliverNewMessageNotification(h.cfg, peerWakeNotification("drained"), false, map[string]os.FileInfo{}); err != nil {
		h.t.Fatalf("deliverNewMessageNotification (empty inbox): %v", err)
	}
}

func (h *wakePiBridgeHarness) writeBridgeResult(attemptID string, accepted bool) {
	h.t.Helper()
	dir := filepath.Join(h.root, "agents", "codex", "extensions", "pi-bridge", "doorbell")
	payload, err := json.Marshal(map[string]any{
		"protocol": wakePiBridgeProtocol,
		"accepted": accepted,
		"attempt":  attemptID,
		"at":       time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, attemptID+".result.json"), payload, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *wakePiBridgeHarness) writeBridgeTurn(attemptID, phase string) {
	h.t.Helper()
	dir := filepath.Join(h.root, "agents", "codex", "extensions", "pi-bridge", "doorbell")
	payload, err := json.Marshal(map[string]any{
		"protocol":   wakePiBridgeProtocol,
		"attempt":    attemptID,
		"phase":      phase,
		"generation": h.live.Generation,
		"pid":        h.live.PID,
		"updatedAt":  h.now.UTC().Format(time.RFC3339),
	})
	if err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, attemptID+".turn.json"), payload, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// TestWakePiBridgeResultBeatsClaimProcessingAfterDrain is the stock-pi live
// finding: pi claims and writes its acceptance result before the wake's
// next delivery tick, and a fast drain empties the inbox so no delivery
// tick ever processes the claim. The acceptance still lands in A's own
// ledger, attributed through the attempt's publish-time cohort record,
// and a later message's delivery never re-attributes it.
func TestWakePiBridgeResultBeatsClaimProcessingAfterDrain(t *testing.T) {
	h := newWakePiBridgeHarness(t)
	h.capableBridge()
	h.bridgeDecision = "claimed"
	h.deliverPending()
	h.deliver() // publish A
	h.respond() // pi claims, and its result arrives before the next tick
	attemptA := strings.TrimSuffix(h.publishedRequests()[0], ".json")
	h.writeBridgeResult(attemptA, true)

	// The drain: the inbox empties, so no delivery tick observes the claim.
	h.deliverEmpty()
	h.cfg.collectPiBridgeResults()

	aDetails := strings.Join(h.ledgerDetailsFor("m1"), "\n")
	if !strings.Contains(aDetails, "presented by the pi bridge") {
		t.Fatalf("A's ledger lost the presented record: %q", aDetails)
	}
	if !strings.Contains(aDetails, "accepted by pi (message_start)") {
		t.Fatalf("A's ledger lost the acceptance after the drain: %q", aDetails)
	}
	presentedIdx, acceptedIdx := -1, -1
	for i, d := range h.ledgerDetailsFor("m1") {
		if presentedIdx < 0 && strings.Contains(d, "presented by the pi bridge") {
			presentedIdx = i
		}
		if acceptedIdx < 0 && strings.Contains(d, "accepted by pi (message_start)") {
			acceptedIdx = i
		}
	}
	if presentedIdx < 0 || acceptedIdx < 0 || presentedIdx > acceptedIdx {
		t.Fatalf("the ledger did not persist the presentation before the acceptance: %q", h.ledgerDetailsFor("m1"))
	}

	// The next delivery is a different message: A's acceptance is not
	// recorded again under anyone else's ledger.
	h.cfg.cohortIDs = []string{"m2"}
	files := wakeDoorbellTestFiles(h.t, "pending-b.md")
	h.cfg.doorbell.arm(files)
	h.deliver()
	h.cfg.collectPiBridgeResults()
	if got := strings.Count(h.ledgerLog(), "accepted by pi (message_start)"); got != 1 {
		t.Fatalf("the acceptance was recorded %d times, want once", got)
	}
}

// TestWakePiBridgeAcceptedTurnLeavingMailUnreadReturnsToCadence: a
// recorded acceptance is evidence for the busy rules, not a hold. When
// pi's turn ends without draining — the mail is still new and pi sits
// idle — the cohort's own cadence and caps own the reminders: asks resume
// on the ladder's backoff, each accepted presentation advances the
// unchanged-cohort budget, the budget parks the cohort at four, and the
// wake types nothing through every tick.
func TestWakePiBridgeAcceptedTurnLeavingMailUnreadReturnsToCadence(t *testing.T) {
	h := newWakePiBridgeHarness(t)
	h.capableBridge()
	h.bridgeDecision = "claimed"
	h.deliverPending()
	h.deliver() // publish A
	h.respond() // pi claims and accepts before the next tick
	attemptA := strings.TrimSuffix(h.publishedRequests()[0], ".json")
	h.writeBridgeResult(attemptA, true)

	h.deliver() // the collector records the rows and advances the cadence once
	if got := strings.Count(h.ledgerLog(), "accepted by pi (message_start)"); got != 1 {
		t.Fatalf("the acceptance was recorded %d times, want once", got)
	}

	// Several ticks of the ladder: each accepted presentation advances the
	// budget, the backoff spaces the asks, and nothing ever types.
	asks, lastID := 0, ""
	for i := 0; i < 26; i++ {
		h.advance(2 * time.Second)
		h.deliver()
		if got := h.typedChunks(); len(got) != 0 {
			t.Fatalf("tick %d typed into the terminal: %q", i, got)
		}
		if names := h.publishedRequests(); len(names) == 1 {
			id := strings.TrimSuffix(names[0], ".json")
			if id != lastID {
				asks++
				lastID = id
				h.respond() // pi claims and accepts the re-ask before the next tick
				h.writeBridgeResult(id, true)
			}
		}
	}
	if asks != 3 {
		t.Fatalf("re-asks on the ladder = %d, want three (with the first ask: the budget of four)", asks)
	}
	if got := strings.Count(h.ledgerLog(), "accepted by pi (message_start)"); got != 4 {
		t.Fatalf("acceptances recorded = %d, want one per presented attempt", got)
	}
	if h.cfg.doorbell.phase != wakeDoorbellParked {
		t.Fatalf("phase = %v, want the budget to park the cohort", h.cfg.doorbell.phase)
	}
}

// TestWakePiBridgeClaimedAskHoldsThroughASlowTurn is the stock-pi live
// finding, in pi's real shape: pi claims AND accepts (message_start)
// before the next wake tick, attests its turn running, and only then
// spends the model turn. The attestation holds the busy terminal while it
// says running — even when a second message arrives mid-turn and the
// pending ids change, the failure that killed the id-keyed hold — and the
// unsent draft stays untouched until the turn attests its end and drains
// the mail once.
func TestWakePiBridgeClaimedAskHoldsThroughASlowTurn(t *testing.T) {
	h := newWakePiBridgeHarness(t)
	h.capableBridge()
	h.bridgeDecision = "claimed"
	h.deliverPending()
	h.deliver() // publish A
	h.respond() // pi claims AND accepts before the next tick
	attemptA := strings.TrimSuffix(h.publishedRequests()[0], ".json")
	h.writeBridgeResult(attemptA, true)
	h.writeBridgeTurn(attemptA, "running")

	h.deliver() // the collector records the rows and advances the cohort's cadence
	if got := h.typedChunks(); len(got) != 0 {
		t.Fatalf("an accepted turn got the terminal typed: %q", got)
	}
	if got := strings.Count(h.ledgerLog(), "accepted by pi (message_start)"); got != 1 {
		t.Fatalf("the acceptance was recorded %d times, want once", got)
	}

	// The turn runs. A second message arrives mid-turn, so the pending ids
	// grow past the accepted attempt's own ids; the attestation, not an id
	// key, is what holds. The ladder's base delays the next ask past the
	// 2s decision recheck.
	h.bridgeDecision = "refused"
	h.advance(2 * time.Second)
	h.deliver()
	if got := h.publishedRequests(); len(got) != 0 {
		t.Fatalf("the wake re-asked before the ladder's base: %v", got)
	}
	// The second physical message lands mid-turn; the addition pulls the
	// ladder forward and the next ask covers the grown cohort.
	h.pending = wakeDoorbellTestFiles(h.t, "pending.md", "second.md")
	h.advance(3 * time.Second)
	h.deliver() // publish B
	if got := len(h.publishedRequests()); got != 1 {
		t.Fatalf("asks on disk %v, want the ladder-paced re-ask", got)
	}
	h.respond() // pi refuses: its turn still runs
	h.advance(2 * time.Second)
	h.deliver() // the refusal reconciles; the running attestation holds
	if got := h.typedChunks(); len(got) != 0 {
		t.Fatalf("a refusal pasted over an accepted turn: %q", got)
	}

	// Well past input_max_hold: the hold persists while pi attests the
	// turn running, and the wake keeps asking instead of typing.
	h.advance(20 * time.Second)
	h.deliver()
	if got := h.typedChunks(); len(got) != 0 {
		t.Fatalf("the terminal pasted over the accepted turn: %q", got)
	}

	// The turn attests its end and drains the mail once; the hold is spent
	// and nothing types. The unsent draft was never touched.
	h.writeBridgeTurn(attemptA, "ended")
	h.deliverEmpty()
	if got := h.typedChunks(); len(got) != 0 {
		t.Fatalf("the doorbell typed during the turn: %q", got)
	}
}

// TestWakePiBridgeEndedTurnFollowsTheRatifiedBusyBound: once pi attests
// the doorbell's turn ended, the running-turn exception is spent. A later
// busy turn that has nothing to do with the doorbell — the user's own
// work, with the message still unread — follows the configured
// input_max_hold and the terminal fallback runs, exactly as without any
// acceptance.
func TestWakePiBridgeEndedTurnFollowsTheRatifiedBusyBound(t *testing.T) {
	h := newWakePiBridgeHarness(t)
	h.capableBridge()
	h.cfg.settings.inputMaxHold = 15 * time.Second
	h.bridgeDecision = "claimed"
	h.deliverPending()
	h.deliver() // publish A
	h.respond() // pi claims and accepts
	attemptA := strings.TrimSuffix(h.publishedRequests()[0], ".json")
	h.writeBridgeResult(attemptA, true)
	h.writeBridgeTurn(attemptA, "running")
	h.deliver() // rows recorded, cadence advanced

	// The doorbell's turn ends without draining; pi then starts unrelated
	// work and refuses every re-ask.
	h.writeBridgeTurn(attemptA, "ended")
	h.bridgeDecision = "refused"
	h.advance(5 * time.Second)
	h.deliver() // the ladder's base: publish B
	if got := len(h.publishedRequests()); got != 1 {
		t.Fatalf("asks on disk %v, want the ladder-paced re-ask", got)
	}
	h.respond() // pi refuses: unrelated work
	h.advance(2 * time.Second)
	h.deliver() // the attestation says ended: the busy clock starts
	if got := h.typedChunks(); len(got) != 0 {
		t.Fatalf("the busy bound typed early: %q", got)
	}

	// The full input_max_hold passes: the next refusal reconciles past the
	// bound and the ratified fallback owns the doorbell.
	h.advance(15 * time.Second)
	h.deliver() // the bound is over; the wake asks once more
	h.respond() // pi still refuses
	h.advance(2 * time.Second)
	h.deliver() // the refusal reconciles past the expired bound
	if !h.fullLineTyped() {
		t.Fatalf("the ended turn did not release the busy bound: %q", h.typedChunks())
	}
}

// TestWakePiBridgeTurnFileRetirement: an ended attestation, and a running
// one stale past the ask hold, go with their group's retirement; a fresh
// running attestation outlives the group because the busy hold still
// reads it after the group's other files are spent.
func TestWakePiBridgeTurnFileRetirement(t *testing.T) {
	dir := t.TempDir()
	ended := time.Now().Add(-time.Hour)
	running := time.Now()
	writeTurn := func(id string, phase string, at time.Time) {
		payload, err := json.Marshal(map[string]any{
			"protocol":   wakePiBridgeProtocol,
			"attempt":    id,
			"phase":      phase,
			"generation": "gen-1",
			"pid":        4242,
			"updatedAt":  at.UTC().Format(time.RFC3339),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, id+".turn.json"), payload, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeTurn("doorbell-a", "ended", ended)
	writeTurn("doorbell-b", "running", running)
	writeTurn("doorbell-c", "running", ended)
	pruneWakePiBridgeAttempts(dir, nil, nil, time.Now())

	if _, err := os.Stat(filepath.Join(dir, "doorbell-a.turn.json")); !os.IsNotExist(err) {
		t.Fatal("an ended attestation outlived its group")
	}
	if _, err := os.Stat(filepath.Join(dir, "doorbell-b.turn.json")); err != nil {
		t.Fatal("a fresh running attestation was retired with its group")
	}
	if _, err := os.Stat(filepath.Join(dir, "doorbell-c.turn.json")); !os.IsNotExist(err) {
		t.Fatal("a stale running attestation outlived its group")
	}
}

// TestWakePiBridgeForeignTurnRecordDoesNotHold: a running attestation that
// names another pi process — a replacement after a crash — is not evidence
// for this process's busy hold; the ratified busy bound owns the refusal.
func TestWakePiBridgeForeignTurnRecordDoesNotHold(t *testing.T) {
	h := newWakePiBridgeHarness(t)
	h.capableBridge()
	h.bridgeDecision = "claimed"
	h.deliverPending()
	h.deliver() // publish A
	h.respond() // pi claims and accepts
	attemptA := strings.TrimSuffix(h.publishedRequests()[0], ".json")
	h.writeBridgeResult(attemptA, true)
	h.writeBridgeTurn(attemptA, "running")
	h.deliver() // rows recorded, cadence advanced

	// pi crashes; a replacement generation serves the same seat and refuses.
	h.live.Generation = "gen-2"
	h.bridgeDecision = "refused"
	h.advance(5 * time.Second)
	h.deliver() // the ladder's base: publish B
	h.respond() // the replacement refuses
	h.advance(2 * time.Second)
	h.deliver() // the attestation names gen-1: no hold, the bound owns it
	if !h.fullLineTyped() {
		t.Fatalf("a foreign-generation running record held the busy bound: %q", h.typedChunks())
	}
}

// TestWakePiBridgeStaleLivenessReleasesTheClaim: a claim whose bridge goes
// away holds through the heartbeat's noise, then releases to the terminal
// fallback with the full line, and the dead claim's files are retired so
// later mail can ask again.
func TestWakePiBridgeStaleLivenessReleasesTheClaim(t *testing.T) {
	h := newWakePiBridgeHarness(t)
	h.capableBridge()
	h.bridgeDecision = "claimed"
	h.deliverPending()
	h.deliver() // publish A
	h.respond() // pi claims
	h.deliver() // presented

	h.advance(5 * time.Second) // the ladder's base: the held claim is read again
	h.live = nil
	h.deliver() // one stale read is heartbeat noise: hold
	if got := h.typedChunks(); len(got) != 0 {
		t.Fatalf("a single stale heartbeat typed the terminal: %q", got)
	}

	h.advance(11 * time.Second)
	h.deliver() // the staleness persisted: the claim retires, the cadence owns the reminder
	if got := h.typedChunks(); len(got) != 0 {
		t.Fatalf("the retired claim typed into the terminal: %q", got)
	}
	if got := h.doorbellDirFileCount(); got != 0 {
		t.Fatalf("the dead claim's group was not retired: %d files remain", got)
	}

	// The next pass issues a fresh ask and the bridge stays gone; once the
	// ask's own deadline passes unanswered, the terminal fallback owns the
	// doorbell again.
	h.advance(11 * time.Second)
	h.deliver()
	if !h.fullLineTyped() {
		t.Fatalf("the terminal fallback did not own the full line: %q", h.typedChunks())
	}
}
