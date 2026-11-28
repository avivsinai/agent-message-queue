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
	// After the decision deadline the wake may withdraw an unanswered ask.
	wakePiBridgeDecisionDeadline = 2 * time.Second
	// The ledger mode label that tells a bridge attempt from a typed one.
	wakePiBridgeAttemptMode = "pi-bridge"
	// The retention bound for doorbell attempts (docs/pi-bridge-protocol.md,
	// doorbell): at most the newest 32 attempts stay on disk.
	wakePiBridgeAttemptKeep = 32
	// How long an issued ask holds before it is proven lost: a claim's
	// result may wait for pi's whole turn, far past the decision deadline.
	wakePiBridgeAskHold = 10 * time.Minute
	// How long the bridge's liveness must stay stale before an issued ask
	// counts as lost: one missed heartbeat is noise, not a dead session.
	wakePiBridgeStaleRelease = 10 * time.Second
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
	err error
	// engaged: the wake reached the exchange (a fresh capable record and a
	// valid owner proof), whether or not a decision arrived. The caller
	// types nothing on an engaged pass without presented or fullLine.
	engaged bool
	// presented: the bridge claimed the attempt, so the fixed doorbell is on
	// its way to pi; the wake types nothing and records the presentation.
	presented bool
	// busyHold: a busy refusal is within its bound, a claim is held, or
	// Pi attests that an accepted turn still runs. Recheck on a later tick.
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
func publishWakePiBridgeRequest(dir string, req wakePiBridgeRequest, beforePublish func() error) error {
	payload, err := json.Marshal(req)
	if err != nil {
		return err
	}
	out := filepath.Join(dir, "doorbell")
	if err := os.MkdirAll(out, 0o700); err != nil {
		return err
	}
	created, err := fsq.CreateNewFileIf(filepath.Join(out, req.Attempt+".json"), payload, 0o600, beforePublish)
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
// call, and the call never waits. An already-issued ask is reconciled from
// its on-disk decision first, whatever the current mail is and whether the
// bridge still looks fresh: a claimed ask holds until its result lands or
// its claim is proven lost, a refusal feeds the busy rules, and a deadline
// passed without a decision hands the authority to the withdrawn create.
// Fresh liveness and the owner proof gate only the publishing of a NEW
// ask. A pass with the ask still open returns engaged or holding: the
// caller types nothing and the ask finishes on a later tick.
func (cfg *wakeConfig) runPiBridgeDoorbellAttempt(output wakePayload) wakePiBridgeDoorbellResult {
	if cfg == nil || cfg.wakeOwner == nil || cfg.injectVia != "" ||
		(cfg.injectMode != wakeInjectModeRaw && cfg.injectMode != wakeInjectModePaste) {
		return wakePiBridgeDoorbellResult{}
	}
	now := cfg.wakeDoorbellNow()
	live, ok := cfg.piBridgeReadLiveness(now)
	liveFresh := ok && live.PID > 0 && live.Generation != "" && live.Surface == "tui" && live.Doorbell
	engaged := wakePiBridgeDoorbellResult{engaged: true}
	dir := filepath.Join(cfg.root, "agents", cfg.me, "extensions", "pi-bridge")
	ids := append([]string(nil), cfg.cohortIDs...)
	sort.Strings(ids)
	cohortKey := strings.Join(ids, "\x00")
	doorbellDir := filepath.Join(dir, "doorbell")
	if cfg.piBridgeAttemptsProcessed == nil {
		cfg.piBridgeAttemptsProcessed = map[string]bool{}
	}
	if cfg.piBridgeCadenceDone == nil {
		cfg.piBridgeCadenceDone = map[string]bool{}
	}

	// An issued ask is authoritative whatever the mail and liveness gates
	// say: reconcile it from disk before any new-ask gate runs.
	pending, found := newestWakePiBridgeCohortAttempt(doorbellDir, wakePiBridgeAttemptPrefix(ids))
	if found {
		return cfg.reconcilePiBridgeAsk(dir, doorbellDir, pending, cohortKey, live, liveFresh, now, output)
	}
	if !liveFresh {
		return wakePiBridgeDoorbellResult{}
	}
	if err := cfg.piBridgeOwnerProofFor(live.PID); err != nil {
		return wakePiBridgeDoorbellResult{}
	}
	pruneWakePiBridgeAttempts(doorbellDir, cfg.piBridgeResultsSeen, cfg.piBridgeAttemptsProcessed, now)
	attemptID := wakePiBridgeAttemptID(ids, now)
	// Attribution is established before the ask is observable: a claim and
	// its result must never exist without their message ids, so the cohort
	// record lands first and a failed write publishes nothing. The physical
	// ids are known here, before any claim exists — a result can arrive,
	// and a drain can preempt the delivery tick, before the wake observes
	// its own ask's claim.
	rec := wakePiBridgeCohort{Protocol: wakePiBridgeProtocol, Attempt: attemptID, MessageIDs: ids}
	payload, err := json.Marshal(rec)
	if err != nil {
		return engaged
	}
	if err := os.MkdirAll(doorbellDir, 0o700); err != nil {
		return engaged
	}
	if _, err := fsq.CreateNewFile(
		filepath.Join(doorbellDir, attemptID+".cohort.json"),
		payload, 0o600,
	); err != nil {
		return engaged
	}
	req := wakePiBridgeRequest{
		Protocol:   wakePiBridgeProtocol,
		Op:         "doorbell",
		Attempt:    attemptID,
		Generation: live.Generation,
		PID:        live.PID,
		Deadline:   now.Add(wakePiBridgeDecisionDeadline).UTC().Format(time.RFC3339Nano),
	}
	if err := publishWakePiBridgeRequest(dir, req, cfg.checkPeerDispatch); err != nil {
		if isWakeDispatchRefusal(err) {
			return wakePiBridgeDoorbellResult{err: err}
		}
		// A fresh id plus an error means the directory is unwritable or a
		// raced peer took the id; nothing is proven, so nothing is typed.
		// The cohort record alone retires on the next prune.
		return engaged
	}
	return engaged
}

// reconcilePiBridgeAsk finishes one issued ask from its on-disk decision:
// a claim holds until its result lands or the claim is proven lost, a
// refusal feeds the busy rules, an expired silence hands the authority to
// the withdrawn create, and an unreadable decision proves no winner.
func (cfg *wakeConfig) reconcilePiBridgeAsk(dir, doorbellDir, pending, cohortKey string, live wakePiBridgeLiveness, liveFresh bool, now time.Time, output wakePayload) wakePiBridgeDoorbellResult {
	engaged := wakePiBridgeDoorbellResult{engaged: true}
	decision, reason, present := readWakePiBridgeDecisionState(doorbellDir, pending)
	if !present {
		// The ask is still open. Past the request deadline the exclusive
		// create of "withdrawn" settles this attempt. If it loses the
		// race, honor the bridge's decision.
		if !wakePiBridgeRequestExpired(filepath.Join(doorbellDir, pending+".json"), now) {
			return engaged
		}
		created, err := writeWakePiBridgeWithdrawn(dir, pending)
		if created {
			cfg.piBridgeAttemptsProcessed[pending] = true
			// Withdrawal settles this ask. The earlier accepted turn can
			// still own the composer while its bridge misses the deadline.
			if cfg.piBridgeTurnHoldsBusy(live, liveFresh, now) {
				return cfg.holdTimedOutPiTurn(output, now)
			}
			return wakePiBridgeDoorbellResult{
				fullLine: true,
				reason:   "pi bridge did not answer the doorbell in time",
			}
		}
		if err != nil {
			return engaged
		}
		decision, reason = readWakePiBridgeDecision(doorbellDir, pending)
		present = decision != ""
	}
	if !present {
		// A decision file exists but proves no winner (unreadable,
		// foreign): type nothing this pass, and let the next ask publish a
		// fresh attempt; the decision is never rewritten.
		cfg.piBridgeAttemptsProcessed[pending] = true
		return engaged
	}
	switch decision {
	case "claimed":
		return cfg.reconcilePiBridgeClaim(doorbellDir, pending, cohortKey, liveFresh, now)
	case "refused", "failed":
		cfg.piBridgeAttemptsProcessed[pending] = true
		if cfg.piBridgeTurnHoldsBusy(live, liveFresh, cfg.wakeDoorbellNow()) {
			// pi attests the doorbell's own turn is still running, so the
			// busy bound must not run to a terminal paste over it. The
			// cadence still re-asks on the ladder; the hold ends when pi
			// attests the turn ended, the liveness goes stale, or the ask
			// hold passes — then the ordinary busy rules own the outcome
			// again, including a turn that has nothing to do with the
			// doorbell.
			return wakePiBridgeDoorbellResult{busyHold: true, reason: "pi's doorbell turn is still running"}
		}
		return cfg.piBridgeBusyOutcome(cohortKey, reason, cfg.wakeDoorbellNow())
	default:
		cfg.piBridgeAttemptsProcessed[pending] = true
		return engaged
	}
}

// reconcilePiBridgeClaim holds a claimed ask until its result lands: the
// claim, not the request deadline, is the ask's clock, because pi's turn
// may take far longer than the decision deadline to reach its
// message_start. A claim is never converted to a terminal fallback: an
// elapsed ask hold or a stale heartbeat proves no non-delivery. When the
// hold passes or the liveness stays stale well past one heartbeat, the
// unresolved claim's group is retired and the cohort's own cadence and
// caps own the next independent reminder.
func (cfg *wakeConfig) reconcilePiBridgeClaim(doorbellDir, attemptID, cohortKey string, liveFresh bool, now time.Time) wakePiBridgeDoorbellResult {
	cfg.clearPiBridgeBusy(cohortKey)
	if piBridgeClaimLost(doorbellDir, attemptID, now) || !cfg.piBridgeLiveFresh(cohortKey, liveFresh, now) {
		removeWakePiBridgeGroup(doorbellDir, attemptID, cfg.piBridgeResultsSeen, cfg.piBridgeAttemptsProcessed)
		return wakePiBridgeDoorbellResult{busyHold: true, reason: "pi's doorbell claim went unanswered"}
	}
	if cfg.piBridgeAttemptsProcessed[attemptID] && cfg.piBridgeCadenceDone[attemptID] {
		// Presented, recorded, and on the cohort's cadence; hold until the
		// result arrives.
		return wakePiBridgeDoorbellResult{busyHold: true, reason: "pi holds the doorbell"}
	}
	if cfg.piBridgeCadenceDone == nil {
		cfg.piBridgeCadenceDone = map[string]bool{}
	}
	cfg.piBridgeCadenceDone[attemptID] = true
	return wakePiBridgeDoorbellResult{presented: true, attemptID: attemptID}
}

// piBridgeLiveFresh decides whether the bridge's liveness still backs an
// issued ask. One stale or failed read is inside the heartbeat's noise;
// only staleness that persists past wakePiBridgeStaleRelease releases the
// ask.
func (cfg *wakeConfig) piBridgeLiveFresh(cohortKey string, fresh bool, now time.Time) bool {
	if cfg.piBridgeLiveStaleSince == nil {
		cfg.piBridgeLiveStaleSince = map[string]time.Time{}
	}
	if fresh {
		delete(cfg.piBridgeLiveStaleSince, cohortKey)
		return true
	}
	since, ok := cfg.piBridgeLiveStaleSince[cohortKey]
	if !ok {
		cfg.piBridgeLiveStaleSince[cohortKey] = now
		return true
	}
	if now.Before(since.Add(wakePiBridgeStaleRelease)) {
		return true
	}
	delete(cfg.piBridgeLiveStaleSince, cohortKey)
	return false
}

// piBridgeClaimLost reports a claim that produced no result within the ask
// hold past its own request deadline. The deadline lives inside the
// request, so the bound follows the wake clock the request was published
// with, not file timestamps.
func piBridgeClaimLost(doorbellDir, attemptID string, now time.Time) bool {
	data, err := os.ReadFile(filepath.Join(doorbellDir, attemptID+".json"))
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
	return now.After(deadline.Add(wakePiBridgeAskHold))
}

// removeWakePiBridgeGroup retires one attempt group the wake has proven
// dead: its claim can never produce evidence, so the files go and the
// tracking maps forget it.
func removeWakePiBridgeGroup(dir, attemptID string, seen, processed map[string]bool) {
	for _, suffix := range []string{".json", ".decision.json", ".cohort.json", ".result.json", ".turn.json"} {
		_ = os.Remove(filepath.Join(dir, attemptID+suffix))
	}
	if seen != nil {
		delete(seen, attemptID+".result.json")
	}
	if processed != nil {
		delete(processed, attemptID)
	}
}

// newestWakePiBridgeCohortAttempt returns the cohort's newest request on
// disk, decided or not, acted on or not: the ask this tick reconciles.
func newestWakePiBridgeCohortAttempt(dir string, prefix string) (string, bool) {
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
		if strings.HasSuffix(name, ".decision.json") || strings.HasSuffix(name, ".result.json") ||
			strings.HasSuffix(name, ".cohort.json") || strings.HasSuffix(name, ".turn.json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
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

// wakePiBridgeAcceptedTurn is the wake's memory of the latest accepted
// doorbell attempt. The pending ids say nothing about which turn pi is
// running — mail can arrive during the doorbell's turn and a user can
// start unrelated work after it — so the busy hold follows pi's own turn
// attestation for this attempt, not the cohort key.
type wakePiBridgeAcceptedTurn struct {
	attemptID     string
	heldSince     time.Time
	attentionSent bool
}

// holdTimedOutPiTurn keeps the input hold tied to the accepted turn. New
// asks and changes in its waiting mail cannot restart the attention bound.
func (cfg *wakeConfig) holdTimedOutPiTurn(output wakePayload, now time.Time) wakePiBridgeDoorbellResult {
	turn := cfg.piBridgeLastAccepted
	if turn.heldSince.IsZero() {
		turn.heldSince = now
		_ = writeWakeDiagnostic(cfg, "amq wake: pi turn running; doorbell held, nothing typed\n")
	}
	if !turn.attentionSent && !now.Before(turn.heldSince.Add(cfg.settings.inputMaxHold)) {
		// Attention is the intended non-input channel while Pi owns the
		// composer. Keep the bridge on its ordinary busy recheck schedule.
		if err := deliverWakeAttendedNotice(cfg, output); err != nil {
			return wakePiBridgeDoorbellResult{err: err}
		}
		turn.attentionSent = true
	}
	return wakePiBridgeDoorbellResult{busyHold: true}
}

// piBridgeTurnHoldsBusy reports whether pi attests the accepted
// doorbell's turn is still running. A missing, ended, unreadable, or
// foreign-generation attestation is no hold: an older extension, an
// unrelated busy turn, a crashed extension, and a replaced pi process all
// fall back to the ratified busy bound. The attestation never stops a
// re-ask; the cohort's cadence and caps own the reminders.
//
// The freshness bound is the ask hold, not input_max_hold: a legitimate
// model turn can outlive a short input_max_hold, so the running-turn
// exception must not expire mid-turn; the ask hold already bounds the
// claim the same way, and a crashed extension's stale "running" file
// releases to the ordinary busy rules after it.
func (cfg *wakeConfig) piBridgeTurnHoldsBusy(live wakePiBridgeLiveness, ok bool, now time.Time) bool {
	if !ok || cfg.piBridgeLastAccepted == nil {
		return false
	}
	turn, err := readWakePiBridgeTurnState(
		filepath.Join(cfg.root, "agents", cfg.me, "extensions", "pi-bridge", "doorbell"),
		cfg.piBridgeLastAccepted.attemptID,
	)
	if err != nil || turn == nil || turn.Phase != "running" {
		return false
	}
	if turn.Generation != live.Generation || turn.PID != live.PID {
		return false // the running turn belongs to a process that is gone
	}
	return now.Before(turn.UpdatedAt.Add(wakePiBridgeAskHold))
}

// piBridgeAttemptAccounting advances the cohort's cadence and budget for
// one presented bridge attempt, exactly once: the same ladder and caps
// that pace a typed attempt pace the next independent reminder, so an
// accepted turn that leaves the mail unread is re-asked on the ladder and
// stops at the unchanged-cohort budget.
func (cfg *wakeConfig) piBridgeAttemptAccounting(now time.Time) {
	if len(cfg.doorbell.cohort) > 0 {
		cfg.doorbell.recordAttempt(now)
	}
	persistWakeDoorbellStatusBestEffort(cfg)
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
// own claim named — never to the mail a later delivery happens to carry.
// The presentation is persisted first, and a result is recorded only after
// its attempt's presentation landed and only when its protocol and attempt
// name match the claim it answers. A result is marked seen only after its
// ledger record landed, so a failed recording is retried on a later tick.
// Control flow never waits.
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
	if cfg.piBridgeAttemptsProcessed == nil {
		cfg.piBridgeAttemptsProcessed = map[string]bool{}
	}
	// Presentation first: a claimed decision whose presented row is not yet
	// recorded is reconciled here, whatever the current mail is and whether
	// the bridge still looks fresh. The row attributes through the
	// attempt's own publish-time cohort record; the cohort's cadence still
	// advances on the delivery path that plans the attempt.
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".decision.json") {
			continue
		}
		id := strings.TrimSuffix(name, ".decision.json")
		if !wakePiBridgeAttemptShape(id) || cfg.piBridgeAttemptsProcessed[id] {
			continue
		}
		if decision, _, _ := readWakePiBridgeDecisionState(dir, id); decision != "claimed" {
			continue
		}
		ids, idsErr := readWakePiBridgeCohort(dir, id)
		if idsErr != nil || len(ids) == 0 {
			continue // stays unprocessed: recorded on a later tick
		}
		if cfg.recordPiBridgePresentedRow(dir, id, ids) {
			// The claim advances the cohort's cadence and budget exactly
			// once, here, before the group's later retirement: otherwise a
			// fast claim and result are counted in the ledger but never in
			// the wake scheduler, and every decision recheck publishes
			// another ask.
			cfg.piBridgeAttemptAccounting(cfg.wakeDoorbellNow())
		}
	}
	// Then the results, each validated against the claim it answers and
	// recorded only after that claim's presentation row exists.
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
		if err != nil || res.Protocol != wakePiBridgeProtocol || res.Attempt != attemptID {
			// Unreadable or not this attempt's result: nothing here is
			// evidence yet, so nothing is recorded and the file stays on
			// disk unseen.
			continue
		}
		ids, idsErr := readWakePiBridgeCohort(dir, attemptID)
		if idsErr != nil || len(ids) == 0 {
			// No cohort record names this result: it cannot be attributed,
			// and attributing it to the current mail is exactly the defect.
			// The file stays on disk unseen — a record written before the
			// ask was observable means only a crash window lands here — and
			// a later cohort write (a raced publisher finishing) is picked
			// up.
			continue
		}
		if decision, _, _ := readWakePiBridgeDecisionState(dir, attemptID); decision != "claimed" || !cfg.piBridgeAttemptsProcessed[attemptID] {
			// The presentation row must land first.
			continue
		}
		detail := ""
		if res.Accepted {
			detail = fmt.Sprintf("doorbell attempt %s accepted by pi (message_start)", res.Attempt)
		} else {
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
		if res.Accepted {
			cfg.piBridgeLastAccepted = &wakePiBridgeAcceptedTurn{attemptID: attemptID}
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
	for id := range cfg.piBridgeCadenceDone {
		if _, err := os.Stat(filepath.Join(dir, id+".json")); err != nil {
			delete(cfg.piBridgeCadenceDone, id)
		}
	}
	pruneWakePiBridgeAttempts(dir, cfg.piBridgeResultsSeen, cfg.piBridgeAttemptsProcessed, cfg.wakeDoorbellNow())
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
// claim whose result the ledger recorded; or by being a cohort record whose
// request never published, since no result can name it. An undecided
// request and an unreadable decision stay: the ask still owes them a wake
// action, and the retention cap never evicts an outstanding group.
func pruneWakePiBridgeAttempts(dir string, seen map[string]bool, processed map[string]bool, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type group struct {
		id       string
		files    []string
		mod      time.Time
		request  bool
		finished bool
	}
	groups := map[string]*group{}
	var order []string
	for _, entry := range entries {
		name := entry.Name()
		id := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(name, ".json"), ".decision"), ".result"), ".cohort"), ".turn")
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
		case name == id+".json":
			g.request = true
		}
	}
	for _, id := range order {
		g := groups[id]
		if !g.request && !g.finished && len(g.files) == 1 {
			// A cohort record whose request never published: nothing can
			// attribute to it and nothing is owed.
			g.finished = true
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
			// A turn attestation outlives its group while it still says
			// running and fresh: the busy hold reads it after the group's
			// other files are spent. An ended attestation, and a running
			// one stale past the ask hold, go with the group.
			if strings.HasSuffix(file, ".turn.json") && keepWakePiBridgeTurnFile(file, now) {
				continue
			}
			_ = os.Remove(file)
		}
		if seen != nil {
			delete(seen, g.id+".result.json")
		}
	}
}

// keepWakePiBridgeTurnFile reports whether a running attestation is still
// live evidence: ended, unreadable, or ask-hold-stale files are spent.
func keepWakePiBridgeTurnFile(file string, now time.Time) bool {
	turn, err := readWakePiBridgeTurnFile(file)
	if err != nil || turn == nil || turn.Phase != "running" {
		return false
	}
	return now.Before(turn.UpdatedAt.Add(wakePiBridgeAskHold))
}

// readWakePiBridgeTurnFile reads one turn attestation file.
func readWakePiBridgeTurnFile(file string) (*wakePiBridgeTurnState, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var turn wakePiBridgeTurnState
	if err := json.Unmarshal(data, &turn); err != nil {
		return nil, err
	}
	if turn.Protocol != wakePiBridgeProtocol || turn.Generation == "" || turn.PID <= 0 || turn.UpdatedAt.IsZero() {
		return nil, fmt.Errorf("turn attestation %q does not validate", filepath.Base(file))
	}
	return &turn, nil
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

// readWakePiBridgeTurnState reads pi's turn attestation for an accepted
// attempt: the extension writes phase running when the doorbell's turn
// begins and phase ended when it completes. A missing file is an older
// extension or a turn that never attested; both are no hold.
// wakePiBridgeTurnState is one turn attestation: the extension writes
// phase running when the accepted attempt's turn begins and phase ended
// when it completes. Generation and pid name the pi process that owns the
// running turn, so a replacement process's fresh heartbeat is never held
// by an old record.
type wakePiBridgeTurnState struct {
	Protocol   string    `json:"protocol"`
	Attempt    string    `json:"attempt"`
	Phase      string    `json:"phase"`
	Generation string    `json:"generation"`
	PID        int       `json:"pid"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

func readWakePiBridgeTurnState(dir, attemptID string) (*wakePiBridgeTurnState, error) {
	if !wakePiBridgeAttemptShape(attemptID) {
		return nil, fmt.Errorf("bad attempt id %q", attemptID)
	}
	turn, err := readWakePiBridgeTurnFile(filepath.Join(dir, attemptID+".turn.json"))
	if err != nil {
		return nil, err
	}
	if turn.Attempt != attemptID {
		return nil, fmt.Errorf("turn attestation for %q names attempt %q", attemptID, turn.Attempt)
	}
	return turn, nil
}

// recordPiBridgePresented records a claimed attempt as a presented
// doorbell: the cohort's cadence and caps advance exactly as after a typed
// presentation, the submit-only state stays false (nothing was typed), and
// the attempt lands in the wake ledger under its own mode label so trace
// can tell a bridge attempt from a typed one. The claim is never labeled
// accepted: the acceptance evidence is recorded when its result arrives.
func (cfg *wakeConfig) recordPiBridgePresented(now time.Time, currentPending map[string]os.FileInfo, attemptID string) {
	if len(cfg.doorbell.cohort) == 0 {
		cfg.doorbell.arm(currentPending)
	}
	// The claim's cohort association is written at publish time, so the
	// later-arriving result already names the mail this claim presented,
	// whatever a later delivery carries.
	ids, err := readWakePiBridgeCohort(
		filepath.Join(cfg.root, "agents", cfg.me, "extensions", "pi-bridge", "doorbell"), attemptID,
	)
	if err != nil || len(ids) == 0 {
		ids = cfg.cohortIDs
	}
	if cfg.recordPiBridgePresentedRow(
		filepath.Join(cfg.root, "agents", cfg.me, "extensions", "pi-bridge", "doorbell"), attemptID, ids,
	) {
		// Exactly once per attempt, on the row landing: the collector may
		// already have recorded this row and advanced the cadence on an
		// earlier tick.
		cfg.piBridgeAttemptAccounting(now)
	}
}

// recordPiBridgePresentedRow writes the presented ledger row once per
// attempt, attributed to the ids the attempt's own cohort record names.
// Until the row lands the attempt stays unprocessed, so a failed write is
// retried on a later tick.
func (cfg *wakeConfig) recordPiBridgePresentedRow(dir, attemptID string, ids []string) bool {
	if cfg.piBridgeAttemptsProcessed == nil {
		cfg.piBridgeAttemptsProcessed = map[string]bool{}
	}
	if cfg.piBridgeAttemptsProcessed[attemptID] || len(ids) == 0 {
		return false
	}
	writer := notificationattempt.NewWriter(cfg.root, cfg.me)
	prepared, prepareErr := writer.Prepare(ids, wakePiBridgeAttemptMode)
	if prepareErr != nil && !notificationattempt.IsRotationOnly(prepareErr) {
		return false
	}
	if err := writer.Result(prepared, notificationattempt.OutcomeWritten,
		fmt.Sprintf("doorbell presented by the pi bridge (attempt %s)", attemptID)); err != nil {
		return false
	}
	cfg.piBridgeAttemptsProcessed[attemptID] = true
	return true
}
