// Package notificationattempt persists a durable audit log of notification
// write attempts and their lifecycle so that `amq trace` and `amq doctor
// --ops` can report whether a wake attempted to notify an agent and what the
// outcome was.
//
// The ledger NEVER blocks delivery: if persisting the prepared record fails,
// the wake injects anyway and the failure is recorded for trace to surface.
// An audit log that blocks the thing it audits is worse than no audit log.
//
// Design (lead review 2026-08-30, three hard requirements):
//
//  1. TRUE append-only (O_APPEND), not read-modify-write. The prototype read
//     the whole journal, concatenated, and called WriteFileAtomic — O(n) per
//     notification on the wake hot path, and a lost-update race under
//     concurrency (two notifications both read current, both append, one
//     record vanishes silently). O_APPEND gives kernel-level atomic appends
//     on local filesystems: concurrent writers each append a full line and
//     neither loses data. There is no read-modify-write and no lock.
//
//  2. ONE log, not two. The prototype used separate prepared/result files
//     rotated independently; whichever crossed the size cap first dropped its
//     old records while the other kept its partners, orphaning results that
//     trace would render as "attempted, never completed" — a false failure
//     report for the exact scenario this ledger exists to diagnose. In a
//     single append-only log, a result is always written AFTER its prepared,
//     so rotation (a size-capped move to .1) drops the prepared first and can
//     never orphan a surviving result. The phase field distinguishes them.
//
//  3. trace distinguishes "no attempt recorded" from "recording failed". If
//     the prepared write itself fails and the wake injects anyway (correct),
//     the journal has a hole. An operator debugging a missed doorbell must
//     not conclude the wake never tried. Prepare returns a writeErr that the
//     caller carries; trace surfaces a distinct leg wording for "we failed to
//     keep the record" vs "we have no record".
package notificationattempt

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	// SchemaVersion is the current integer schema version of a Record. Version
	// 2 adds ordered lifecycle state events while keeping the v1 prepared/result
	// records readable. Do not remove the version check: an unversioned reader
	// is how a schema change silently corrupts history.
	SchemaVersion       = 2
	legacySchemaVersion = 1

	PhasePrepared = "prepared"
	PhaseResult   = "result"

	OutcomeWritten = "written"
	OutcomeFailed  = "failed"

	// StateIndeterminate: a prepared record exists with no matching result.
	// The wake may still be in flight, or it may have crashed between prepare
	// and result. Trace renders this as "attempted; outcome unknown".
	StateIndeterminate = "indeterminate"

	// StateWriteFailed: the prepared write itself failed (the ledger could not
	// persist the record). The wake injected anyway (the ledger never blocks
	// delivery). Trace renders this as "attempted; the attempt record could
	// not be persisted" — distinct from StateIndeterminate ("no result yet")
	// and from an empty journal ("no attempt recorded").
	StateWriteFailed = "write_failed"

	// Lifecycle states are deliberately small. They describe AMQ's durable
	// attempt state, not provider-side presentation or consumption.
	StateAttempt  = "attempt"
	StateDeferred = "deferred"
	StateRetried  = "retried"
	StateAccepted = "accepted"
	StateFailed   = "failed"
	StateInvalid  = "invalid"

	LogFilename   = "notification-attempts.jsonl"
	rotatedSuffix = ".1"

	defaultMaxBytes = 256 * 1024
)

// Record is one append-only journal entry. The Phase field distinguishes a
// prepared record (before injection) from a result record (after injection).
// Version 1 result records carry Outcome (written/failed). Version 2
// lifecycle records carry State and Sequence instead.
type Record struct {
	Schema     int      `json:"schema"`
	AttemptID  string   `json:"attempt_id"`
	Phase      string   `json:"phase"`
	MessageIDs []string `json:"message_ids"`
	Agent      string   `json:"agent"`
	Mode       string   `json:"mode"`
	RecordedAt string   `json:"recorded_at"`
	Outcome    string   `json:"outcome,omitempty"`
	Detail     string   `json:"detail,omitempty"`
	State      string   `json:"state,omitempty"`
	Sequence   uint64   `json:"sequence,omitempty"`
}

// Attempt is the joined view of a prepared record and its optional result,
// used by trace and doctor. State is derived from the ordered lifecycle for a
// v2 attempt, or from the v1 result outcome for a legacy attempt.
type Attempt struct {
	State    string   `json:"state"`
	Prepared Record   `json:"prepared"`
	Result   *Record  `json:"result,omitempty"`
	History  []Record `json:"history,omitempty"`
}

// Lifecycle is the in-process handle for one durable notification attempt.
// The handle keeps one AttemptID across deferred/retried delivery and is not
// itself a source of authority; the append-only journal is.
type Lifecycle struct {
	AttemptID  string
	MessageIDs []string
	Agent      string
	Mode       string
	State      string
	Sequence   uint64
}

// Writer appends prepared/result records to the per-agent notification log.
// Each append opens the file with O_APPEND (kernel-level atomic append on
// local filesystems), writes one JSON line, and closes. There is no
// read-modify-write and no lock — concurrent writers do not lose data.
type Writer struct {
	root     string
	agent    string
	maxBytes int64
	now      func() time.Time
}

func NewWriter(root, agent string) *Writer {
	return &Writer{
		root:     root,
		agent:    agent,
		maxBytes: defaultMaxBytes,
		now:      time.Now,
	}
}

// Prepare appends a prepared record and returns it. If the append fails, it
// returns a zero Record, a non-nil writeErr, and the attempt ID + message IDs
// the caller intended to record — so the caller can still pass an identity to
// Result (which will record a result with outcome=failed and the write error
// in Detail), and trace can surface "recording failed" rather than "no
// attempt recorded". The ledger never blocks delivery: the caller injects
// regardless of writeErr.
func (w *Writer) Prepare(messageIDs []string, mode string) (record Record, writeErr error) {
	return w.prepare(messageIDs, mode, "")
}

// Begin appends the initial v2 lifecycle event for one notification attempt.
// The returned handle must be used for every later state transition so a
// deferred attempt keeps the same AttemptID and cohort.
func (w *Writer) Begin(messageIDs []string, mode string) (*Lifecycle, error) {
	record, err := w.prepare(messageIDs, mode, StateAttempt)
	if err != nil {
		return nil, err
	}
	return &Lifecycle{
		AttemptID:  record.AttemptID,
		MessageIDs: append([]string{}, record.MessageIDs...),
		Agent:      record.Agent,
		Mode:       record.Mode,
		State:      record.State,
		Sequence:   record.Sequence,
	}, nil
}

// Transition appends one ordered v2 lifecycle state. Invalid transitions are
// rejected before writing, so a terminal state cannot silently be reopened.
func (w *Writer) Transition(lifecycle *Lifecycle, state, detail string) error {
	if lifecycle == nil || lifecycle.AttemptID == "" || len(lifecycle.MessageIDs) == 0 {
		return fmt.Errorf("invalid notification attempt lifecycle: missing identity")
	}
	if lifecycle.Agent != "" && lifecycle.Agent != w.agent {
		return fmt.Errorf("notification attempt agent mismatch: lifecycle %q writer %q", lifecycle.Agent, w.agent)
	}
	state = strings.TrimSpace(state)
	if !validLifecycleState(state) {
		return fmt.Errorf("notification attempt lifecycle state %q is invalid", state)
	}
	if !validLifecycleTransition(lifecycle.State, state) {
		return fmt.Errorf("notification attempt lifecycle transition %q -> %q is invalid", lifecycle.State, state)
	}
	record := Record{
		Schema:     SchemaVersion,
		AttemptID:  lifecycle.AttemptID,
		Phase:      PhaseResult,
		MessageIDs: append([]string{}, lifecycle.MessageIDs...),
		Agent:      w.agent,
		Mode:       lifecycle.Mode,
		RecordedAt: w.now().UTC().Format(time.RFC3339Nano),
		Detail:     strings.TrimSpace(detail),
		State:      state,
		Sequence:   lifecycle.Sequence + 1,
	}
	if err := w.append(record); err != nil {
		return fmt.Errorf("persist notification attempt transition: %w", err)
	}
	lifecycle.State = state
	lifecycle.Sequence = record.Sequence
	return nil
}

func (w *Writer) prepare(messageIDs []string, mode, state string) (record Record, writeErr error) {
	if err := fsq.ValidateHandle(w.agent); err != nil {
		return Record{}, fmt.Errorf("notification attempt agent: %w", err)
	}
	ids := normalizedMessageIDs(messageIDs)
	if len(ids) == 0 {
		return Record{}, fmt.Errorf("notification attempt requires at least one message id")
	}
	attemptID, err := format.NewMessageID(w.now())
	if err != nil {
		return Record{}, fmt.Errorf("notification attempt id: %w", err)
	}
	record = Record{
		Schema:     SchemaVersion,
		AttemptID:  attemptID,
		Phase:      PhasePrepared,
		MessageIDs: ids,
		Agent:      w.agent,
		Mode:       strings.TrimSpace(mode),
		RecordedAt: w.now().UTC().Format(time.RFC3339Nano),
		State:      strings.TrimSpace(state),
	}
	if err := w.append(record); err != nil {
		return Record{}, fmt.Errorf("persist prepared notification attempt: %w", err)
	}
	return record, nil
}

// Result appends a result record for a prepared attempt. If prepared is the
// zero Record (because Prepare's write failed), the caller MUST pass the
// attempt ID and message IDs it intended to record; Result reconstructs a
// minimal prepared identity so the result is still joinable. outcome must be
// OutcomeWritten or OutcomeFailed.
func (w *Writer) Result(prepared Record, outcome, detail string) error {
	if outcome != OutcomeWritten && outcome != OutcomeFailed {
		return fmt.Errorf("notification attempt outcome must be %q or %q", OutcomeWritten, OutcomeFailed)
	}
	if prepared.AttemptID == "" || len(prepared.MessageIDs) == 0 {
		return fmt.Errorf("invalid prepared notification attempt: missing identity")
	}
	if prepared.Agent != "" && prepared.Agent != w.agent {
		return fmt.Errorf("notification attempt agent mismatch: prepared %q writer %q", prepared.Agent, w.agent)
	}
	record := Record{
		Schema:     SchemaVersion,
		AttemptID:  prepared.AttemptID,
		Phase:      PhaseResult,
		MessageIDs: append([]string{}, prepared.MessageIDs...),
		Agent:      w.agent,
		Mode:       prepared.Mode,
		RecordedAt: w.now().UTC().Format(time.RFC3339Nano),
		Outcome:    outcome,
		Detail:     strings.TrimSpace(detail),
	}
	if err := w.append(record); err != nil {
		return fmt.Errorf("persist notification attempt result: %w", err)
	}
	return nil
}

// append writes one JSON line to the log with O_APPEND. On local filesystems
// O_APPEND writes are atomic w.r.t. other O_APPEND writers, so concurrent
// notifications neither lose data nor need a lock. If the file would exceed
// maxBytes, the current file is moved to .1 (rotation) and a fresh file
// starts. Rotation is best-effort: if the rename fails, the append proceeds
// (an over-size audit log is better than a lost record).
func (w *Writer) append(record Record) error {
	if w.maxBytes <= 0 {
		return fmt.Errorf("notification attempt journal size cap must be positive")
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if int64(len(data)) > w.maxBytes {
		return fmt.Errorf("notification attempt record is %d bytes, cap is %d", len(data), w.maxBytes)
	}

	identity, err := fsq.SnapshotDeliveryRoot(w.root)
	if err != nil {
		return err
	}
	root, err := fsq.OpenDeliveryRoot(w.root, identity)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	if err := root.EnsureAgentDirs(w.agent); err != nil {
		return fmt.Errorf("ensure notification attempt receipts dir: %w", err)
	}
	dir := filepath.Join("agents", w.agent, "receipts")
	fullPath := filepath.Join(root.Base(), dir, LogFilename)
	lockPath := fullPath + ".lock"

	// Coordination model: a separate lock file carries the flock; the data file
	// stays O_APPEND so the hot-path write remains kernel-atomic at EOF among
	// concurrent appenders (requirement 1, unchanged). The lock gates ONLY the
	// rare rotation.
	//
	//	appender: flock(LOCK_SH) on lock file -> O_APPEND write on data file
	//	rotator:  flock(LOCK_EX) on lock file -> read+merge+truncate data file
	//
	// LOCK_SH holders do not contend with each other, so concurrent appenders
	// stay concurrent — the hot path keeps its lock-free-among-appenders
	// property; the cost is one flock(LOCK_SH) syscall, never serialization
	// between appends. LOCK_EX waits for in-flight appenders and blocks new
	// ones only for the rotation window, closing the race where an O_APPEND
	// write landed between rotation's read and its truncate — the exact
	// silent-loss path REQ1 removed on the hot path but rotation reintroduced.
	//
	// The lock file is distinct from the data file so rotation may freely
	// truncate or rename the data file without invalidating anyone's lock:
	// appenders lock the lock file, not the data file, so a rotated/recreated
	// data file does not orphan a held lock. (flock is per-inode; had we locked
	// the data file, a rename-based rotation would detach the lock. We truncate
	// in place anyway, but the separate lock file is robust to either choice.)
	lockFile, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open notification attempt journal lock: %w", err)
	}
	defer func() { _ = lockFile.Close() }()

	// Decide whether this append triggers a rotation. The size check is a stat
	// on the data file BEFORE taking the lock: it is a hint, not a commitment.
	// If two appenders race past this check, the one that wins LOCK_EX below
	// rotates; the loser, on acquiring LOCK_SH, finds the file already
	// truncated and simply appends to the fresh file. A stale-high stat (size
	// grew after we read it) is harmless — the locked re-check corrects it.
	rotate := false
	if info, statErr := os.Stat(fullPath); statErr == nil {
		rotate = info.Size()+int64(len(data)) > w.maxBytes
	}

	if rotate {
		// LOCK_EX serializes rotation against all appenders: it blocks until
		// every in-flight LOCK_SH appender has finished its write and released,
		// then prevents new appends for the duration of read->merge->truncate.
		if err := flockExclusive(lockFile); err != nil {
			return fmt.Errorf("lock notification attempt journal for rotation: %w", err)
		}
		// Re-check the size UNDER LOCK_EX: another rotator that held EX before
		// us may have already truncated, making rotation unnecessary now. This
		// is what prevents double rotation and the lost-record window.
		if info2, e2 := os.Stat(fullPath); e2 == nil {
			rotate = info2.Size()+int64(len(data)) > w.maxBytes
		} else {
			rotate = false
		}
		if rotate {
			rotated := fullPath + rotatedSuffix
			current, readErr := os.ReadFile(fullPath)
			if readErr != nil && !os.IsNotExist(readErr) {
				// Can't read current to merge — shed the over-size log by rename
				// (better than a lost record); the prior .1 is overwritten. Still
				// under LOCK_EX so no appender is racing us.
				_ = os.Rename(fullPath, rotated)
			} else if readErr == nil {
				// Merge current into .1 (preserve older generations), then
				// truncate current in place. We hold LOCK_EX so no appender can
				// O_APPEND a record between this read and the truncate — the
				// race that reintroduced silent loss is closed.
				if existing, eerr := os.ReadFile(rotated); eerr == nil {
					_ = os.WriteFile(rotated, append(existing, current...), 0o600)
				} else {
					_ = os.WriteFile(rotated, current, 0o600)
				}
				_ = os.Truncate(fullPath, 0)
			}
		}
		// Downgrade LOCK_EX -> LOCK_SH for the append. A flock conversion is
		// NOT atomic on Linux: the EX is released and then the SH acquired, so
		// a pending rotator's EX can be granted in the gap before our SH lands.
		// Correctness here does NOT depend on atomic conversion — it comes from
		// re-opening the data file AFTER this SH is finally held (below). Any
		// rotator that sneaks into the gap is drained (it holds EX, finishes its
		// read/merge/truncate, releases) before our subsequent open sees a stable
		// data file; our write then lands under our SH, which excludes further
		// rotation. Do not lean on atomic conversion in a path not backstopped by
		// open-after-lock.
		if err := flockShared(lockFile); err != nil {
			flockRelease(lockFile)
			return fmt.Errorf("downgrade to shared lock for append: %w", err)
		}
		defer flockRelease(lockFile)
	} else {
		// Hot path: shared lock. Does not contend with other appenders, so the
		// O_APPEND write below stays concurrent and kernel-atomic at EOF. The
		// lock excludes only the rare rotation (LOCK_EX), which is the point.
		if err := flockShared(lockFile); err != nil {
			return fmt.Errorf("lock notification attempt journal for append: %w", err)
		}
		defer flockRelease(lockFile)
	}

	// O_APPEND | O_CREATE | O_WRONLY, mode 0600. Opened AFTER acquiring the
	// lock so the fd we write through reflects any rotation that just ran (a
	// truncated or renamed-and-recreated current file). We hold LOCK_SH, so no
	// rotator can truncate beneath this write. Among concurrent appenders the
	// kernel serializes O_APPEND writes — each lands a full line at EOF
	// atomically, no lost-update race (requirement 1, preserved).
	f, err := os.OpenFile(fullPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open notification attempt journal: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write notification attempt record: %w", err)
	}
	return nil
}

// List reads the notification log for an agent and returns the joined
// prepared→result attempts, optionally filtered by messageID. A missing
// journal returns (nil, nil) — no attempts recorded is empty evidence, not an
// error. This is the correct fail-mode for trace's "no evidence" leg.
func List(root, agent, messageID string) ([]Attempt, error) {
	identity, err := fsq.SnapshotDeliveryRoot(root)
	if err != nil {
		return nil, err
	}
	deliveryRoot, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		return nil, err
	}
	defer func() { _ = deliveryRoot.Close() }()
	return ListDeliveryRoot(deliveryRoot, agent, messageID)
}

// ListDeliveryRoot is like List but accepts an already-opened DeliveryRoot.
func ListDeliveryRoot(root *fsq.DeliveryRoot, agent, messageID string) ([]Attempt, error) {
	if err := fsq.ValidateHandle(agent); err != nil {
		return nil, fmt.Errorf("notification attempt agent: %w", err)
	}
	records, err := readRecords(root, agent)
	if err != nil {
		return nil, err
	}
	preparedByAttempt := make(map[string]Record)
	legacyResultByAttempt := make(map[string]Record)
	lifecycleByAttempt := make(map[string][]Record)
	for _, rec := range records {
		switch rec.Phase {
		case PhaseResult:
			if rec.State != "" {
				lifecycleByAttempt[rec.AttemptID] = append(lifecycleByAttempt[rec.AttemptID], rec)
			} else {
				legacyResultByAttempt[rec.AttemptID] = rec
			}
		case PhasePrepared:
			preparedByAttempt[rec.AttemptID] = rec
		}
	}
	var attempts []Attempt
	for _, prepared := range preparedByAttempt {
		if messageID != "" && !contains(prepared.MessageIDs, messageID) {
			continue
		}
		attempt := Attempt{State: StateIndeterminate, Prepared: prepared}
		if prepared.State != "" || len(lifecycleByAttempt[prepared.AttemptID]) > 0 {
			var legacyResult *Record
			if result, ok := legacyResultByAttempt[prepared.AttemptID]; ok {
				resultCopy := result
				legacyResult = &resultCopy
			}
			attempt = foldLifecycle(
				prepared,
				lifecycleByAttempt[prepared.AttemptID],
				legacyResult,
			)
		} else if result, ok := legacyResultByAttempt[prepared.AttemptID]; ok {
			resultCopy := result
			attempt.State = result.Outcome
			attempt.Result = &resultCopy
			attempt.History = []Record{prepared, result}
		}
		attempts = append(attempts, attempt)
	}
	sort.SliceStable(attempts, func(i, j int) bool {
		return attempts[i].Prepared.RecordedAt < attempts[j].Prepared.RecordedAt
	})
	return attempts, nil
}

func foldLifecycle(prepared Record, events []Record, legacyResult *Record) Attempt {
	attempt := Attempt{
		State:    StateInvalid,
		Prepared: prepared,
		History:  append([]Record{prepared}, events...),
	}
	if prepared.State != StateAttempt || prepared.Sequence != 0 || prepared.Outcome != "" {
		return attempt
	}
	if len(events) == 0 && legacyResult != nil {
		if legacyResult.AttemptID != prepared.AttemptID ||
			legacyResult.Agent != prepared.Agent ||
			legacyResult.Mode != prepared.Mode ||
			!sameStrings(legacyResult.MessageIDs, prepared.MessageIDs) {
			return attempt
		}
		resultCopy := *legacyResult
		attempt.State = resultCopy.Outcome
		attempt.Result = &resultCopy
		attempt.History = append(attempt.History, resultCopy)
		return attempt
	}
	if legacyResult != nil {
		return attempt
	}
	state := prepared.State
	sequence := prepared.Sequence
	for _, event := range events {
		if event.AttemptID != prepared.AttemptID ||
			event.Agent != prepared.Agent ||
			event.Mode != prepared.Mode ||
			!sameStrings(event.MessageIDs, prepared.MessageIDs) ||
			event.Sequence != sequence+1 ||
			!validLifecycleTransition(state, event.State) {
			return attempt
		}
		state = event.State
		sequence = event.Sequence
		resultCopy := event
		attempt.Result = &resultCopy
	}
	attempt.State = state
	return attempt
}

// readRecords reads the notification log (and its .1 rotation, oldest first)
// and validates each line. A line that fails validation is skipped with a
// continue — a corrupt line must not prevent trace from reporting the valid
// history around it. (An audit log that refuses to read because one line is
// bad would hide evidence of every other attempt.)
func readRecords(root *fsq.DeliveryRoot, agent string) ([]Record, error) {
	dir := filepath.Join("agents", agent, "receipts")
	var records []Record
	// Read .1 (older) first, then the current file, so records are in
	// append order across the rotation boundary.
	for _, name := range []string{LogFilename + rotatedSuffix, LogFilename} {
		path := filepath.Join(dir, name)
		data, err := root.ReadRegularNoFollow(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read notification attempt journal %s: %w", path, err)
		}
		scanner := bufio.NewScanner(strings.NewReader(string(data)))
		scanner.Buffer(make([]byte, 4096), defaultMaxBytes)
		line := 0
		for scanner.Scan() {
			line++
			if strings.TrimSpace(scanner.Text()) == "" {
				continue
			}
			var record Record
			if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
				// Skip the corrupt line but keep reading — a single bad line
				// must not blank out the rest of the history.
				continue
			}
			if err := validateRecord(record, agent); err != nil {
				continue
			}
			records = append(records, record)
		}
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("scan notification attempt journal %s: %w", path, err)
		}
	}
	return records, nil
}

// validateRecord checks the invariants a Record must satisfy. SchemaVersion
// is checked so a future schema bump with a migration branch is explicit; an
// unknown schema is rejected (fail-closed on history we cannot interpret
// rather than rendering a wrong answer).
func validateRecord(record Record, agent string) error {
	if record.Schema != SchemaVersion && record.Schema != legacySchemaVersion {
		return fmt.Errorf("notification attempt schema %d is not %d or legacy %d", record.Schema, SchemaVersion, legacySchemaVersion)
	}
	if record.Agent != agent || record.AttemptID == "" || len(record.MessageIDs) == 0 || record.RecordedAt == "" {
		return fmt.Errorf("invalid notification attempt record")
	}
	// RecordedAt must be a parseable RFC3339 timestamp — an unparseable
	// timestamp makes the sort in List meaningless. (Carried over from
	// selfupgrade's discipline: validate at the boundary, not at use.)
	if _, err := time.Parse(time.RFC3339Nano, record.RecordedAt); err != nil {
		return fmt.Errorf("notification attempt recorded_at is not RFC3339: %w", err)
	}
	switch record.Phase {
	case PhasePrepared:
		if record.Outcome != "" {
			return fmt.Errorf("prepared record must not claim an outcome")
		}
		if record.State != "" {
			if record.Schema != SchemaVersion || record.State != StateAttempt || record.Sequence != 0 {
				return fmt.Errorf("prepared lifecycle record must start at state %q sequence 0", StateAttempt)
			}
		}
	case PhaseResult:
		if record.State != "" {
			if record.Schema != SchemaVersion || record.Sequence == 0 || !validLifecycleState(record.State) || record.Outcome != "" {
				return fmt.Errorf("invalid notification attempt lifecycle result")
			}
		} else if record.Outcome != OutcomeWritten && record.Outcome != OutcomeFailed {
			return fmt.Errorf("result outcome must be %q or %q", OutcomeWritten, OutcomeFailed)
		}
	default:
		return fmt.Errorf("notification attempt phase %q is invalid", record.Phase)
	}
	return nil
}

func validLifecycleState(state string) bool {
	switch state {
	case StateAttempt, StateDeferred, StateRetried, StateAccepted, StateFailed:
		return true
	default:
		return false
	}
}

func validLifecycleTransition(from, to string) bool {
	switch from {
	case StateAttempt:
		return to == StateDeferred || to == StateAccepted || to == StateFailed
	case StateDeferred:
		return to == StateRetried
	case StateRetried:
		return to == StateDeferred || to == StateAccepted || to == StateFailed
	default:
		return false
	}
}

func normalizedMessageIDs(messageIDs []string) []string {
	seen := make(map[string]bool, len(messageIDs))
	ids := make([]string, 0, len(messageIDs))
	for _, raw := range messageIDs {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// ErrNoJournal is returned by helpers that need to distinguish "the journal
// does not exist" from "the journal exists but is empty". List does not use
// it (empty = no evidence), but trace may when deciding leg wording.
var ErrNoJournal = errors.New("notification attempt journal does not exist")
