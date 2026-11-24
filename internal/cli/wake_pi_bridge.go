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
	// The decision poll and the busy retry gap.
	wakePiBridgePollInterval = 100 * time.Millisecond
	wakePiBridgeBusyRetryGap = time.Second
	wakePiBridgeMaxAttempts  = 32
	// The ledger mode label that tells a bridge attempt from a typed one.
	wakePiBridgeAttemptMode = "pi-bridge"
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
	// presented: the bridge claimed the attempt, so the fixed doorbell is on
	// its way to pi; the wake types nothing and records the presentation.
	presented bool
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

func (cfg *wakeConfig) piBridgeSleepFor(d time.Duration) {
	if cfg.piBridgeSleep != nil {
		cfg.piBridgeSleep(d)
		return
	}
	time.Sleep(d)
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

// wakePiBridgeAttemptID mints the attempt id for one cohort attempt: a
// durable id derived from the cohort's message ids and a sequence number,
// so repeated reminders of the same cohort write distinct files and a new
// wake never collides with a dead wake's attempt.
func wakePiBridgeAttemptID(ids []string, seq int) string {
	h := sha256.Sum256([]byte(strings.Join(ids, "\x00")))
	return fmt.Sprintf("doorbell-%x-%d", h[:8], seq)
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
	data, err := os.ReadFile(filepath.Join(dir, "doorbell", attemptID+".decision.json"))
	if err != nil {
		return "", ""
	}
	var dec wakePiBridgeDecision
	if err := json.Unmarshal(data, &dec); err != nil {
		return "", ""
	}
	if dec.Protocol != wakePiBridgeProtocol || dec.Attempt != attemptID {
		return "", ""
	}
	return dec.Decision, dec.Reason
}

// writeWakePiBridgeWithdrawn publishes the wake's decision for one attempt:
// the wake won the race, the attempt is over, and a late bridge that reads
// it does nothing.
func writeWakePiBridgeWithdrawn(dir string, attemptID string) {
	dec := wakePiBridgeDecision{
		Protocol: wakePiBridgeProtocol,
		Attempt:  attemptID,
		Decision: "withdrawn",
	}
	payload, err := json.Marshal(dec)
	if err != nil {
		return
	}
	_, _ = fsq.CreateNewFile(filepath.Join(dir, "doorbell", attemptID+".decision.json"), payload, 0o600)
}

// runPiBridgeDoorbellAttempt is the wake side of the exchange. It returns
// presented=true when the bridge claimed the attempt (the cohort is
// presented; the caller records it and writes nothing to the terminal),
// and otherwise a result whose reason says why the terminal fallback runs.
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
	dir := filepath.Join(cfg.root, "agents", cfg.me, "extensions", "pi-bridge")
	ids := append([]string(nil), cfg.cohortIDs...)
	sort.Strings(ids)

	hold := cfg.settings.inputMaxHold
	if hold < 0 {
		hold = 0
	}
	holdUntil := now.Add(hold)

	for seq := 1; seq <= wakePiBridgeMaxAttempts; seq++ {
		attemptID := wakePiBridgeAttemptID(ids, seq)
		req := wakePiBridgeRequest{
			Protocol:   wakePiBridgeProtocol,
			Op:         "doorbell",
			Attempt:    attemptID,
			Generation: live.Generation,
			PID:        os.Getpid(),
			Deadline:   now.Add(wakePiBridgeDecisionDeadline).UTC().Format(time.RFC3339Nano),
		}
		if err := publishWakePiBridgeRequest(dir, req); err != nil {
			// The id is taken (a dead wake's attempt or a raced peer);
			// never overwrite — try the next sequence number.
			continue
		}
		decision, reason := "", ""
		deadline := cfg.wakeDoorbellNow().Add(wakePiBridgeDecisionDeadline)
		for {
			decision, reason = readWakePiBridgeDecision(dir, attemptID)
			if decision != "" {
				break
			}
			current := cfg.wakeDoorbellNow()
			if !current.Before(deadline) {
				break
			}
			cfg.piBridgeSleepFor(wakePiBridgePollInterval)
		}
		switch decision {
		case "claimed":
			return wakePiBridgeDoorbellResult{presented: true, attemptID: attemptID}
		case "refused", "failed":
			if !cfg.wakeDoorbellNow().Before(holdUntil) {
				return wakePiBridgeDoorbellResult{
					fullLine: true,
					reason:   fmt.Sprintf("pi bridge busy: %s", strings.TrimSpace(reason)),
				}
			}
			cfg.piBridgeSleepFor(wakePiBridgeBusyRetryGap)
			continue
		default:
			// No decision in time: the wake withdraws, and a bridge that
			// lost the read-back race does nothing. Terminal fallback.
			writeWakePiBridgeWithdrawn(dir, attemptID)
			if d, _ := readWakePiBridgeDecision(dir, attemptID); d == "claimed" {
				// The bridge wrote its claim between our deadline and the
				// withdrawal; the doorbell is on its way to pi.
				return wakePiBridgeDoorbellResult{presented: true, attemptID: attemptID}
			}
			return wakePiBridgeDoorbellResult{
				fullLine: true,
				reason:   "pi bridge did not answer the doorbell in time",
			}
		}
	}
	return wakePiBridgeDoorbellResult{
		fullLine: true,
		reason:   "pi bridge attempts were exhausted",
	}
}

// recordPiBridgePresented records a claimed attempt as a presented
// doorbell: the cohort's cadence and caps advance exactly as after a typed
// presentation, the submit-only state stays false (nothing was typed), and
// the attempt lands in the wake ledger under its own mode label so trace
// can tell a bridge attempt from a typed one.
func (cfg *wakeConfig) recordPiBridgePresented(now time.Time, currentPending map[string]os.FileInfo, attemptID string) {
	recordWakeAttempt(cfg, now, currentPending, nil)
	persistWakeDoorbellStatusBestEffort(cfg)
	if len(cfg.cohortIDs) == 0 {
		return
	}
	writer := notificationattempt.NewWriter(cfg.root, cfg.me)
	prepared, prepareErr := writer.Prepare(cfg.cohortIDs, wakePiBridgeAttemptMode)
	if prepareErr != nil && !notificationattempt.IsRotationOnly(prepareErr) {
		return
	}
	detail := fmt.Sprintf("doorbell accepted by the pi bridge (attempt %s)", attemptID)
	if _, err := os.Stat(filepath.Join(cfg.root, "agents", cfg.me, "extensions", "pi-bridge", "doorbell", attemptID+".result.json")); err == nil {
		detail += "; acceptance result published"
	}
	_ = writer.Result(prepared, notificationattempt.OutcomeWritten, detail)
}
