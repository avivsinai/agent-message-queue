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
	live     *wakePiBridgeLiveness
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
			strings.HasSuffix(name, ".result.json") || strings.HasSuffix(name, ".cohort.json") {
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
		if strings.HasSuffix(e.Name(), ".json") && !strings.HasSuffix(e.Name(), ".decision.json") &&
			!strings.HasSuffix(e.Name(), ".result.json") && !strings.HasSuffix(e.Name(), ".cohort.json") {
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
	current := wakeDoorbellTestFiles(h.t, "pending.md")
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
	h.presentViaBridge()
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
	if got := h.doorbellDirFileCount(); got != 1 {
		t.Fatalf("doorbell dir holds %d files after the retired refusal, want only a2's request", got)
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
	pruneWakePiBridgeAttempts(dir, seen, processed)

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
