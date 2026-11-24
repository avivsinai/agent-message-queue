// The pi bridge doorbell: one wake delivery face for a pi seat whose bridge
// (integrations/pi/amq-bridge.ts, revision 7) serves the doorbell exchange.
// The wake asks an idle, serving pi to show the fixed doorbell as a visible
// custom message that starts one turn, so this path types nothing into the
// terminal. See docs/pi-bridge-protocol.md, "Bridge revision 7: the wake
// doorbell", and the ratified decisions for bead
// agent-message-queue-e1gx: a claimed attempt counts as a presented doorbell
// for the cohort's cadence and caps, and it never sets the submit-only
// state, because nothing was typed — the first terminal write for that
// cohort, if a later reminder falls back, is always the full fixed line.
package cli

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/notificationattempt"
)

// wakePiBridgeProtocol is the pi bridge wire protocol (internal/remote/pi
// ProtocolV1); the literal keeps the wake's small reader self-contained.
const wakePiBridgeProtocol = "amq:pi-bridge:v1"

const (
	// The bridge replaces bridge.liveness every 2 seconds; 5 seconds of age
	// is the adapter's staleness bound, and the wake uses the same.
	wakePiBridgeHeartbeatMaxAge = 5 * time.Second
	// One decision deadline per published attempt: the bridge claims or
	// refuses at once, so anything past this is no bridge serving.
	wakePiBridgeDecisionDeadline = 2 * time.Second
	// The ledger mode label that tells a bridge attempt from a typed one.
	wakePiBridgeAttemptMode = "pi-bridge"
	// The retention bound for doorbell attempts (docs/pi-bridge-protocol.md,
	// doorbell): at most the newest 32 attempts stay on disk.
	wakePiBridgeAttemptKeep = 32
)

// wakePiBridgeLiveness is the part of bridge.liveness the doorbell needs.
type wakePiBridgeLiveness struct {
	PID        int
	Generation string
	Surface    string
	Doorbell   bool
}

// wakePiBridgeDoorbellResult is what one bridge exchange decided.
type wakePiBridgeDoorbellResult struct {
	// engaged: the wake reached the exchange (a fresh capable record and a
	// valid owner proof), whether or not a decision arrived. The caller
	// types nothing on an engaged pass without presented or fullLine.
	engaged bool
	// presented: the bridge claimed the attempt, so the fixed doorbell is on
	// its way to pi; the wake types nothing and records the presentation.
	presented bool
	// busyHold: the bridge refused busy and the cohort's input_max_hold
	// bound is still open; the wake types nothing and asks again on a later
	// tick, with a new attempt and a fresh deadline.
	busyHold bool
	// fullLine: the wake ran the bridge and it did not present, so a
	// terminal fallback this attempt must write the full fixed line, never a
	// submit-only Enter (nothing the bridge did put bytes in the terminal).
	fullLine bool
	// reason: why the bridge did not present, for the fallback line.
	reason string
	// attemptID: the claimed attempt, for the ledger detail.
	attemptID string
}

// piBridgeReadLiveness, piBridgeOwnerProof and piBridgeSleep are the test
// seams; nil falls through to the real read, the real owner proof and the
// real sleep.
func (cfg *wakeConfig) piBridgeReadLiveness(now time.Time) (wakePiBridgeLiveness, bool) {
	if cfg.piBridgeLivenessRead != nil {
		return cfg.piBridgeLivenessRead(now)
	}
	return readWakePiBridgeLiveness(cfg.root, cfg.me, now)
}

func (cfg *wakeConfig) piBridgeOwnerProofFor(pid int) error {
	if cfg.piBridgeOwnerProof != nil {
		return cfg.piBridgeOwnerProof(pid)
	}
	owner := cfg.wakeOwner
	if owner == nil {
		return fmt.Errorf("no wake owner")
	}
	if pid != owner.PID {
		return fmt.Errorf("bridge pid %d is not the wake owner pid %d", pid, owner.PID)
	}
	holder := launch.LocalHolder(owner.PID, owner.ProcessStart, owner.BootID)
	if state, why := launch.InspectHolder(holder); state != launch.LeaseValid {
		return fmt.Errorf("wake owner pid %d is no longer that process: %s", owner.PID, why)
	}
	return nil
}

// readWakePiBridgeLiveness reads agents/<me>/extensions/pi-bridge/
// bridge.liveness and reports it only when it is fresh, live, this
// protocol, and publishes the doorbell capability on the terminal surface.
func readWakePiBridgeLiveness(root string, me string, now time.Time) (wakePiBridgeLiveness, bool) {
	dir := filepath.Join(root, "agents", me, "extensions", "pi-bridge")
	st, err := os.Stat(filepath.Join(dir, "bridge.liveness"))
	if err != nil {
		return wakePiBridgeLiveness{}, false
	}
	age := now.Sub(st.ModTime())
	if age < 0 {
		age = 0
	}
	if age > wakePiBridgeHeartbeatMaxAge {
		return wakePiBridgeLiveness{}, false
	}
	data, err := os.ReadFile(filepath.Join(dir, "bridge.liveness"))
	if err != nil {
		return wakePiBridgeLiveness{}, false
	}
	var rec struct {
		Protocol          string `json:"protocol"`
		Live              bool   `json:"live"`
		PID               int    `json:"pid"`
		SessionGeneration string `json:"session_generation"`
		Surface           string `json:"surface"`
		Doorbell          bool   `json:"doorbell"`
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return wakePiBridgeLiveness{}, false
	}
	if rec.Protocol != wakePiBridgeProtocol || !rec.Live || rec.PID <= 0 ||
		rec.SessionGeneration == "" || rec.Surface != "tui" || !rec.Doorbell {
		return wakePiBridgeLiveness{}, false
	}
	return wakePiBridgeLiveness{
		PID:        rec.PID,
		Generation: rec.SessionGeneration,
		Surface:    rec.Surface,
		Doorbell:   rec.Doorbell,
	}, true
}

// wakePiBridgeAttemptPrefix is the file-name prefix of one cohort's
// attempts, so the wake prunes only its own.
func wakePiBridgeAttemptPrefix(ids []string) string {
	h := sha256.Sum256([]byte(strings.Join(ids, "\x00")))
	return fmt.Sprintf("doorbell-%x-", h[:8])
}

// wakePiBridgeAttemptID mints a fresh attempt id for one cohort attempt:
// the cohort's prefix plus the wake clock's nanoseconds, so repeated
// reminders never collide with an earlier attempt's retained files.
func wakePiBridgeAttemptID(ids []string, now time.Time) string {
	return fmt.Sprintf("%s%d", wakePiBridgeAttemptPrefix(ids), now.UnixNano())
}

type wakePiBridgeRequest struct {
	Protocol   string `json:"protocol"`
	Op         string `json:"op"`
	Attempt    string `json:"attempt"`
	Generation string `json:"generation"`
	PID        int    `json:"pid"`
	Deadline   string `json:"deadline"`
}

type wakePiBridgeDecision struct {
	Protocol string `json:"protocol"`
	Attempt  string `json:"attempt"`
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
}

// publishWakePiBridgeRequest writes one request whole with create-new
// semantics (the same publication boundary as the receipts). An existing
// file with the same id is never overwritten: the caller moves to the next
// sequence number.
func publishWakePiBridgeRequest(dir string, req wakePiBridgeRequest) error {
	payload, err := json.Marshal(req)
	if err != nil {
		return err
	}
	out := filepath.Join(dir, "doorbell")
	if err := os.MkdirAll(out, 0o700); err != nil {
		return err
	}
	created, err := fsq.CreateNewFile(filepath.Join(out, req.Attempt+".json"), payload, 0o600)
	if err != nil {
		return err
	}
	if !created {
		return fmt.Errorf("doorbell attempt %s already published", req.Attempt)
	}
	return nil
}

// readWakePiBridgeDecision returns the decision for one attempt, "" when
// none is published yet. A foreign or mismatched record reads as absent,
// never as a decision.
func readWakePiBridgeDecision(dir string, attemptID string) (string, string) {
	decision, reason, _ := readWakePiBridgeDecisionState(dir, attemptID)
	return decision, reason
}

// readWakePiBridgeDecisionState reads one decision file and reports
// whether one exists. A file that exists but proves no winner (unreadable,
// foreign) returns present with an empty decision.
func readWakePiBridgeDecisionState(dir string, attemptID string) (string, string, bool) {
	data, err := os.ReadFile(filepath.Join(dir, attemptID+".decision.json"))
	if err != nil {
		return "", "", false
	}
	var dec wakePiBridgeDecision
	if err := json.Unmarshal(data, &dec); err != nil || dec.Protocol != wakePiBridgeProtocol || dec.Attempt != attemptID {
		return "", "", true
	}
	return dec.Decision, dec.Reason, true
}

// writeWakePiBridgeWithdrawn publishes the wake's decision for one attempt
// with create-new semantics, and reports whether this call created it: the
// create result is the only authority to take the terminal path. A decision
// is never deleted or rewritten.
// piBridgeWithdrawCreate is the exclusive create of the wake's withdrawn
// decision; it is a var so a test can race a late bridge claim into the
// window between the deadline and the create.
var piBridgeWithdrawCreate = fsq.CreateNewFile

func writeWakePiBridgeWithdrawn(dir string, attemptID string) (bool, error) {
	dec := wakePiBridgeDecision{
		Protocol: wakePiBridgeProtocol,
		Attempt:  attemptID,
		Decision: "withdrawn",
	}
	payload, err := json.Marshal(dec)
	if err != nil {
		return false, err
	}
	created, err := piBridgeWithdrawCreate(filepath.Join(dir, "doorbell", attemptID+".decision.json"), payload, 0o600)
	if err != nil {
		return false, err
	}
	return created, nil
}

// runPiBridgeDoorbellAttempt is the wake side of the exchange: ONE ask per
// call, and the call never waits. It finishes the cohort's pending ask (a
// claimed decision presents, a refusal feeds the busy rules, a deadline
// passed without a decision hands the authority to the withdrawn create)
// or publishes one new attempt. A pass with a live ask still open returns
// engaged: the caller types nothing and the ask finishes on a later tick.
func (cfg *wakeConfig) runPiBridgeDoorbellAttempt() wakePiBridgeDoorbellResult {
	if cfg == nil || cfg.wakeOwner == nil || cfg.injectVia != "" ||
		(cfg.injectMode != wakeInjectModeRaw && cfg.injectMode != wakeInjectModePaste) {
		return wakePiBridgeDoorbellResult{}
	}
	now := cfg.wakeDoorbellNow()
	live, ok := cfg.piBridgeReadLiveness(now)
	if !ok || live.PID <= 0 || live.Generation == "" || live.Surface != "tui" || !live.Doorbell {
		return wakePiBridgeDoorbellResult{}
	}
	if err := cfg.piBridgeOwnerProofFor(live.PID); err != nil {
		return wakePiBridgeDoorbellResult{}
	}
	engaged := wakePiBridgeDoorbellResult{engaged: true}
	dir := filepath.Join(cfg.root, "agents", cfg.me, "extensions", "pi-bridge")
	ids := append([]string(nil), cfg.cohortIDs...)
	sort.Strings(ids)
	cohortKey := strings.Join(ids, "\x00")
	doorbellDir := filepath.Join(dir, "doorbell")
	if cfg.piBridgeAttemptsProcessed == nil {
		cfg.piBridgeAttemptsProcessed = map[string]bool{}
	}

	pending, found := newestWakePiBridgeCohortAttempt(doorbellDir, wakePiBridgeAttemptPrefix(ids), cfg.piBridgeAttemptsProcessed)
	if !found {
		pruneWakePiBridgeAttempts(doorbellDir, cfg.piBridgeResultsSeen, cfg.piBridgeAttemptsProcessed)
		attemptID := wakePiBridgeAttemptID(ids, now)
		req := wakePiBridgeRequest{
			Protocol:   wakePiBridgeProtocol,
			Op:         "doorbell",
			Attempt:    attemptID,
			Generation: live.Generation,
			PID:        live.PID,
			Deadline:   now.Add(wakePiBridgeDecisionDeadline).UTC().Format(time.RFC3339Nano),
		}
		if err := publishWakePiBridgeRequest(dir, req); err != nil {
			// A fresh id plus an error means the directory is unwritable or a
			// raced peer took the id; nothing is proven, so nothing is typed.
			return engaged
		}
		return engaged
	}
	done := func() wakePiBridgeDoorbellResult {
		cfg.piBridgeAttemptsProcessed[pending] = true
		return engaged
	}
	decision, reason, present := readWakePiBridgeDecisionState(doorbellDir, pending)
	if !present {
		// The ask is still open. Past the request deadline the exclusive
		// create of "withdrawn" is the only authority to type; when it
		// loses to an existing decision, that decision decides.
		if !wakePiBridgeRequestExpired(filepath.Join(doorbellDir, pending+".json"), now) {
			return engaged
		}
		created, err := writeWakePiBridgeWithdrawn(dir, pending)
		if created {
			cfg.piBridgeAttemptsProcessed[pending] = true
			return wakePiBridgeDoorbellResult{
				fullLine: true,
				reason:   "pi bridge did not answer the doorbell in time",
			}
		}
		if err != nil {
			return engaged
		}
		d, r := readWakePiBridgeDecision(doorbellDir, pending)
		switch d {
		case "claimed":
			cfg.clearPiBridgeBusy(cohortKey)
			cfg.piBridgeAttemptsProcessed[pending] = true
			return wakePiBridgeDoorbellResult{presented: true, attemptID: pending}
		case "refused", "failed":
			cfg.piBridgeAttemptsProcessed[pending] = true
			return cfg.piBridgeBusyOutcome(cohortKey, r, cfg.wakeDoorbellNow())
		default:
			return done()
		}
	}
	switch decision {
	case "claimed":
		cfg.clearPiBridgeBusy(cohortKey)
		cfg.piBridgeAttemptsProcessed[pending] = true
		return wakePiBridgeDoorbellResult{presented: true, attemptID: pending}
	case "refused", "failed":
		cfg.piBridgeAttemptsProcessed[pending] = true
		return cfg.piBridgeBusyOutcome(cohortKey, reason, cfg.wakeDoorbellNow())
	default:
		// A decision file exists but proves no winner (unreadable, foreign):
		// type nothing this pass, and let the next ask publish a fresh
		// attempt; the decision is never rewritten.
		return done()
	}
}

// newestWakePiBridgeCohortAttempt returns the cohort's newest request the
// wake has not acted on yet, decided or not: the ask this tick finishes.
func newestWakePiBridgeCohortAttempt(dir string, prefix string, processed map[string]bool) (string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	newest := ""
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".json") {
			continue
		}
		if strings.HasSuffix(name, ".decision.json") || strings.HasSuffix(name, ".result.json") || strings.HasSuffix(name, ".cohort.json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		if processed[id] {
			continue
		}
		if id > newest {
			newest = id
		}
	}
	return newest, newest != ""
}

// piBridgeBusyOutcome applies the busy rules: the bound starts at the
// cohort's first refusal and equals input_max_hold. Before it passes the
// wake types nothing and asks again on a later tick; when it passes, the
// terminal path runs with a one-line reason. With input_max_hold 0 the
// first refusal falls back at once.
func (cfg *wakeConfig) piBridgeBusyOutcome(cohortKey string, reason string, now time.Time) wakePiBridgeDoorbellResult {
	hold := cfg.settings.inputMaxHold
	if hold < 0 {
		hold = 0
	}
	start := cfg.piBridgeBusyStart(cohortKey, now)
	busy := strings.TrimSpace(reason)
	if busy == "" {
		busy = "the bridge refused the attempt"
	}
	if hold == 0 || !now.Before(start.Add(hold)) {
		cfg.clearPiBridgeBusy(cohortKey)
		why := fmt.Sprintf("pi is busy; using the terminal doorbell (%s)", busy)
		if hold > 0 {
			why = fmt.Sprintf("pi was busy for %d; using the terminal doorbell (%s)", int(hold.Seconds()), busy)
		}
		return wakePiBridgeDoorbellResult{fullLine: true, reason: why}
	}
	return wakePiBridgeDoorbellResult{busyHold: true, reason: busy}
}

// piBridgeBusyStart returns the cohort's first refusal, recording now for
// the first one; only one cohort is ever armed.
func (cfg *wakeConfig) piBridgeBusyStart(cohortKey string, now time.Time) time.Time {
	if cfg.piBridgeBusySince == nil {
		cfg.piBridgeBusySince = map[string]time.Time{}
	}
	for key := range cfg.piBridgeBusySince {
		if key != cohortKey {
			delete(cfg.piBridgeBusySince, key)
		}
	}
	start, ok := cfg.piBridgeBusySince[cohortKey]
	if !ok {
		start = now
		cfg.piBridgeBusySince[cohortKey] = start
	}
	return start
}

func (cfg *wakeConfig) clearPiBridgeBusy(cohortKey string) {
	if cfg.piBridgeBusySince != nil {
		delete(cfg.piBridgeBusySince, cohortKey)
	}
}

// collectPiBridgeResults records the doorbell attempts whose evidence
// arrived after the claim. It runs on ordinary wake ticks, not only when
// mail is being delivered, and each result is attributed to the cohort its
// own claim named — never to the mail a later delivery happens to carry. A
// result is marked seen only after its ledger record landed, so a failed
// recording is retried on a later tick. Control flow never waits.
func (cfg *wakeConfig) collectPiBridgeResults() {
	if cfg == nil {
		return
	}
	dir := filepath.Join(cfg.root, "agents", cfg.me, "extensions", "pi-bridge", "doorbell")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	if cfg.piBridgeResultsSeen == nil {
		cfg.piBridgeResultsSeen = map[string]bool{}
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".result.json") || cfg.piBridgeResultsSeen[name] {
			continue
		}
		attemptID := strings.TrimSuffix(name, ".result.json")
		var res struct {
			Protocol string `json:"protocol"`
			Accepted bool   `json:"accepted"`
			Attempt  string `json:"attempt"`
			Reason   string `json:"reason"`
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err == nil {
			err = json.Unmarshal(data, &res)
		}
		ids, idsErr := readWakePiBridgeCohort(dir, attemptID)
		if idsErr != nil || len(ids) == 0 {
			// No claim cohort to attribute the result to; remember it so the
			// scan does not retry it forever.
			cfg.piBridgeResultsSeen[name] = true
			continue
		}
		detail := ""
		switch {
		case err != nil:
			detail = fmt.Sprintf("doorbell result %s is unreadable: %v", name, err)
		case res.Accepted:
			detail = fmt.Sprintf("doorbell attempt %s accepted by pi (message_start)", res.Attempt)
		default:
			detail = fmt.Sprintf("doorbell attempt %s not delivered: %s", res.Attempt, strings.TrimSpace(res.Reason))
		}
		writer := notificationattempt.NewWriter(cfg.root, cfg.me)
		prepared, prepareErr := writer.Prepare(ids, wakePiBridgeAttemptMode)
		if prepareErr != nil && !notificationattempt.IsRotationOnly(prepareErr) {
			continue // stays unseen: recorded on a later tick
		}
		if resultErr := writer.Result(prepared, notificationattempt.OutcomeWritten, detail); resultErr != nil {
			continue // stays unseen: recorded on a later tick
		}
		cfg.piBridgeResultsSeen[name] = true
	}
	// The tracking maps stay bounded by what is on disk.
	for name := range cfg.piBridgeResultsSeen {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			delete(cfg.piBridgeResultsSeen, name)
		}
	}
	for id := range cfg.piBridgeAttemptsProcessed {
		if _, err := os.Stat(filepath.Join(dir, id+".json")); err != nil {
			delete(cfg.piBridgeAttemptsProcessed, id)
		}
	}
	pruneWakePiBridgeAttempts(dir, cfg.piBridgeResultsSeen, cfg.piBridgeAttemptsProcessed)
}

// wakePiBridgeCohort is the claim-time association between a doorbell
// attempt and the physical mail it presented; the later-arriving result is
// recorded against exactly these message ids.
type wakePiBridgeCohort struct {
	Protocol   string   `json:"protocol"`
	Attempt    string   `json:"attempt"`
	MessageIDs []string `json:"messageIds"`
}

func readWakePiBridgeCohort(dir string, attemptID string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(dir, attemptID+".cohort.json"))
	if err != nil {
		return nil, err
	}
	var rec wakePiBridgeCohort
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, err
	}
	if rec.Protocol != wakePiBridgeProtocol || rec.Attempt != attemptID {
		return nil, fmt.Errorf("cohort record %s does not name its attempt", attemptID)
	}
	return rec.MessageIDs, nil
}

// pruneWakePiBridgeAttempts retires the wake's proven-finished attempt
// groups and bounds them without a slot table. A group is proven finished
// only by a readable decision: refused, failed, a withdrawn create, or a
// claim whose result the ledger recorded. An undecided request and an
// unreadable decision stay: the ask still owes them a wake action, and the
// retention cap never evicts an outstanding group.
func pruneWakePiBridgeAttempts(dir string, seen map[string]bool, processed map[string]bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type group struct {
		id       string
		files    []string
		mod      time.Time
		finished bool
	}
	groups := map[string]*group{}
	var order []string
	for _, entry := range entries {
		name := entry.Name()
		id := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(name, ".json"), ".decision"), ".result"), ".cohort")
		if id == name || !wakePiBridgeAttemptShape(id) {
			continue
		}
		g := groups[id]
		if g == nil {
			g = &group{id: id}
			groups[id] = g
			order = append(order, id)
		}
		g.files = append(g.files, filepath.Join(dir, name))
		if info, err := entry.Info(); err == nil && info.ModTime().After(g.mod) {
			g.mod = info.ModTime()
		}
		switch {
		case strings.HasSuffix(name, ".decision.json"):
			decision, _, _ := readWakePiBridgeDecisionState(dir, id)
			switch decision {
			case "refused", "failed", "withdrawn":
				g.finished = processed != nil && processed[id]
			case "claimed":
				g.finished = seen != nil && seen[id+".result.json"]
			}
		}
	}
	var remove []*group
	for _, id := range order {
		if groups[id].finished {
			remove = append(remove, groups[id])
		}
	}
	// At most the newest wakePiBridgeAttemptKeep finished groups linger;
	// outstanding groups are never evicted.
	if len(remove) > wakePiBridgeAttemptKeep {
		sort.Slice(remove, func(i, j int) bool { return remove[i].mod.After(remove[j].mod) })
		for _, g := range remove[wakePiBridgeAttemptKeep:] {
			g.finished = false
		}
	}
	for _, g := range remove {
		if !g.finished {
			continue
		}
		for _, file := range g.files {
			_ = os.Remove(file)
		}
		if seen != nil {
			delete(seen, g.id+".result.json")
		}
	}
}

// wakePiBridgeAttemptShape bounds an attempt id the way the bridge reader
// does, so a foreign file is never removed.
func wakePiBridgeAttemptShape(id string) bool {
	if !strings.HasPrefix(id, "doorbell-") || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}

func wakePiBridgeRequestExpired(path string, now time.Time) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var req wakePiBridgeRequest
	if json.Unmarshal(data, &req) != nil || req.Protocol != wakePiBridgeProtocol {
		return false
	}
	deadline, err := time.Parse(time.RFC3339Nano, req.Deadline)
	if err != nil {
		return false
	}
	return !now.Before(deadline)
}

// recordPiBridgePresented records a claimed attempt as a presented
// doorbell: the cohort's cadence and caps advance exactly as after a typed
// presentation, the submit-only state stays false (nothing was typed), and
// the attempt lands in the wake ledger under its own mode label so trace
// can tell a bridge attempt from a typed one. The claim is never labeled
// accepted: the acceptance evidence is recorded when its result arrives.
func (cfg *wakeConfig) recordPiBridgePresented(now time.Time, currentPending map[string]os.FileInfo, attemptID string) {
	recordWakeAttempt(cfg, now, currentPending, nil)
	persistWakeDoorbellStatusBestEffort(cfg)
	if len(cfg.cohortIDs) == 0 {
		return
	}
	// The claim's cohort association is written before the presented
	// record: the later-arriving result must name the mail this claim
	// presented, whatever a later delivery carries.
	ids := append([]string(nil), cfg.cohortIDs...)
	sort.Strings(ids)
	rec := wakePiBridgeCohort{Protocol: wakePiBridgeProtocol, Attempt: attemptID, MessageIDs: ids}
	if payload, err := json.Marshal(rec); err == nil {
		_, _ = fsq.CreateNewFile(
			filepath.Join(cfg.root, "agents", cfg.me, "extensions", "pi-bridge", "doorbell", attemptID+".cohort.json"),
			payload, 0o600,
		)
	}
	writer := notificationattempt.NewWriter(cfg.root, cfg.me)
	prepared, prepareErr := writer.Prepare(cfg.cohortIDs, wakePiBridgeAttemptMode)
	if prepareErr != nil && !notificationattempt.IsRotationOnly(prepareErr) {
		return
	}
	_ = writer.Result(prepared, notificationattempt.OutcomeWritten,
		fmt.Sprintf("doorbell presented by the pi bridge (attempt %s)", attemptID))
}
