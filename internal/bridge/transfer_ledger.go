package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// ledgerSessionName maps a receive alias (host/agent) onto one path-safe
// ledger directory name: both components are validated bridge identifiers
// joined with a single underscore, so the session directory stays inside
// bridge/transfer-ledger/ with no traversal.
func ledgerSessionName(receiveAlias string) (string, error) {
	host, agent, err := ParseAlias(receiveAlias)
	if err != nil {
		return "", err
	}
	if len(host)+len(agent)+1 > bridgeIdentifierMaxBytes {
		return "", fmt.Errorf("ledger session name exceeds %d bytes", bridgeIdentifierMaxBytes)
	}
	return host + "_" + agent, nil
}

// TransferLedger is the owning-layer durability primitive for bridge apply
// (design section 7.5, "Apply state machine"). The reused maildir apply path
// (ApplyEnvelope + DeliverToExistingInbox) only checks/publishes inbox/new, so
// a consumer-drained transfer would be re-published on retry: a duplicate the
// Maildir filename alone does not prevent. The ledger records one state
// machine per (source_host, transfer_id) so retries consult durable state
// before re-applying, and the Maildir filename is never treated as a commit
// marker.
//
// States: prepared -> committed | rejected | uncertain.
//
//   - prepared:   intent record, digests bound, appended before ApplyEnvelope.
//   - committed:  verified publication evidence exists (a matched artifact in
//     inbox/new, inbox/cur, or a DLQ envelope whose ORIGINAL bytes hash-match);
//     never operator assertion alone.
//   - rejected:   terminal conflict (os.ErrExist with a different digest).
//   - uncertain:  unknown history. Neither an outcome is emitted nor a blind
//     re-apply happens. It is a refusal, not automatic recovery: surfaced by
//     status/doctor, resolved only when verified evidence later appears (or a
//     separately authorized recovery contract says otherwise). No force
//     controls exist in this package.
//
// Serialization: one OS-level advisory flock per transfer key via the
// OpenLockFile pattern (retained inode, kernel-released on crash, no stale
// sentinel). Torn or ambiguous ledger state is preserved as-is — a record
// that fails to parse is surfaced as uncertain, never treated as absent.
type TransferLedger struct {
	root    *fsq.DeliveryRoot
	session string
	dir     string // root-relative "bridge/transfer-ledger/<session>"
	lockDir string // root-relative "bridge/transfer-ledger/<session>/locks"
}

// LedgerState is the terminal-or-pending disposition of one transfer record.
type LedgerState string

const (
	LedgerPrepared  LedgerState = "prepared"
	LedgerCommitted LedgerState = "committed"
	LedgerRejected  LedgerState = "rejected"
	LedgerUncertain LedgerState = "uncertain"
)

// ledgerRecord is the on-disk JSON record. Appends are one JSON object per
// line; the effective state of a transfer is the LAST valid record for its
// key (earlier records for the same key are history).
type ledgerRecord struct {
	Version       int         `json:"version"`
	State         LedgerState `json:"state"`
	SourceHost    string      `json:"source_host"`
	TransferID    string      `json:"transfer_id"`
	PayloadSHA256 string      `json:"payload_sha256"`
	Reason        string      `json:"reason,omitempty"`
	CommittedPath string      `json:"committed_path,omitempty"`
	RecordedAt    string      `json:"recorded_at"`
}

// LedgerRecord is the caller-facing view of one transfer's ledger entry.
type LedgerRecord struct {
	State         LedgerState
	SourceHost    string
	TransferID    string
	PayloadSHA256 string
	Reason        string
	CommittedPath string
	RecordedAt    string
}

const (
	ledgerSchemaVersion = 1
	ledgerReasonNone    = ""
	// ReasonConflict marks a rejected record produced by ApplyEnvelope's
	// os.ErrExist-with-different-digest translation.
	ledgerReasonConflict = "transfer_conflict"
)

func ledgerRelDir(session string) string {
	return filepath.Join("bridge", "transfer-ledger", session)
}

// NewTransferLedger opens the ledger for one receiver session under an
// already-authorized delivery-root capability.
func NewTransferLedger(root *fsq.DeliveryRoot, receiveAlias string) (*TransferLedger, error) {
	session, err := ledgerSessionName(receiveAlias)
	if err != nil {
		return nil, fmt.Errorf("transfer ledger session: %w", err)
	}
	return &TransferLedger{
		root:    root,
		session: session,
		dir:     ledgerRelDir(session),
		lockDir: filepath.Join(ledgerRelDir(session), "locks"),
	}, nil
}

func ledgerKey(sourceHost, transferID string) string {
	return sourceHost + "-" + transferID
}

func ledgerRecordName(sourceHost, transferID string) string {
	return ledgerKey(sourceHost, transferID) + ".jsonl"
}

func (l *TransferLedger) lockName(sourceHost, transferID string) string {
	return ledgerKey(sourceHost, transferID) + ".lock"
}

// withTransferLock holds the OS-level advisory lock for one transfer key. The
// lock file name is stable and the inode is never replaced, so two concurrent
// courier invocations serialize on the same flock (the OpenLockFile /
// WithDLQEnvelopeLock pattern).
func (l *TransferLedger) withTransferLock(sourceHost, transferID string, fn func() error) error {
	lockFile, err := l.root.OpenLockFile(l.lockDir, l.lockName(sourceHost, transferID), 0o600)
	if err != nil {
		return fmt.Errorf("open transfer lock: %w", err)
	}
	defer func() { _ = lockFile.Close() }()
	return fsq.WithExclusiveFileLock(lockFile, fn)
}

// appendRecord writes one JSON line, fsynced, under the caller-held lock.
// The ledger file is opened for append (create if missing) through the pinned
// capability; the write and its durability sync are one unit.
func (l *TransferLedger) appendRecord(rec ledgerRecord) error {
	if rec.RecordedAt == "" {
		rec.RecordedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return l.root.AppendLedgerLine(l.dir, ledgerRecordName(rec.SourceHost, rec.TransferID), data)
}

// readRecords parses the record file for one transfer key. It returns every
// valid record plus a torn flag: a line that fails to parse (torn/corrupt
// append) sets torn=true and is otherwise skipped — ambiguous state is
// preserved as the torn flag, never silently discarded and never treated as
// absence.
func (l *TransferLedger) readRecords(sourceHost, transferID string) (records []ledgerRecord, torn bool, err error) {
	data, err := l.root.ReadRegularNoFollow(filepath.Join(l.dir, ledgerRecordName(sourceHost, transferID)))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		// A read error is absence of proof, not proof of absence.
		return nil, true, nil
	}
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec ledgerRecord
		if jsonErr := json.Unmarshal([]byte(line), &rec); jsonErr != nil {
			torn = true
			continue
		}
		if rec.Version != ledgerSchemaVersion || rec.State == "" || rec.TransferID != transferID || rec.SourceHost != sourceHost {
			torn = true
			continue
		}
		records = append(records, rec)
	}
	// The effective record is the last valid one; enforce chronological
	// confidence by file order (appends are serialized by the lock).
	return records, torn, nil
}

func (l *TransferLedger) effectiveRecord(sourceHost, transferID string) (rec *ledgerRecord, torn bool, err error) {
	records, torn, err := l.readRecords(sourceHost, transferID)
	if err != nil || torn {
		return nil, torn, err
	}
	if len(records) == 0 {
		return nil, false, nil
	}
	return &records[len(records)-1], false, nil
}

// Digest computes the payload digest the ledger binds into every record.
func Digest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// lookupInBox stats one mailbox box for the deterministic transfer filename.
// Any stat error other than not-exist is propagated as evidence failure.
func lookupInBox(root *fsq.DeliveryRoot, agent, box, filename string) (present bool, evidenceErr bool) {
	path := filepath.Join("agents", agent, "inbox", box, filename)
	_, err := root.Stat(path)
	if err == nil {
		return true, false
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, false
	}
	return false, true
}

// scanDLQForOriginal scans dlq/new and dlq/cur for envelopes whose
// OriginalFile equals the transfer filename and whose ORIGINAL body bytes
// (returned by ReadDLQEnvelope — never the wrapper) hash to wantDigest.
// DLQ delivery creates a new id/filename and wraps the original bytes
// (internal/fsq/dlq.go moveInboxMessageToDLQ), so the transfer filename is
// never the DLQ filename.
func scanDLQForOriginal(root *fsq.DeliveryRoot, agent, filename, wantDigest string) (found bool, evidenceErr bool) {
	for _, box := range []string{"new", "cur"} {
		dir := filepath.Join("agents", agent, "dlq", box)
		entries, err := root.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, true
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			envelope, original, err := fsq.ReadDLQEnvelope(root, path)
			if err != nil {
				// One unreadable DLQ envelope is not proof of absence;
				// treat as evidence failure and fail closed.
				return false, true
			}
			if envelope.OriginalFile != filename {
				continue
			}
			if Digest(original) == wantDigest {
				return true, false
			}
		}
	}
	return false, false
}

// publicationEvidence implements the crash-history A/B recovery rule:
// consult durable publication evidence independent of inbox/new. Evidence is
// conditional and fails closed:
//
//   - inbox/new and inbox/cur: deterministic transfer filename
//     (TransferFilename) looked up directly; a claim never replaces a
//     retained cur copy.
//   - DLQ: envelopes wrap the original bytes under a new id/filename, so
//     match on OriginalFile + original-content digest.
//
// Any read/stat error is treated as absence of proof (evidenceErr), never
// proof of absence.
func publicationEvidence(root *fsq.DeliveryRoot, localAgent, sourceHost, transferID, wantDigest string) (found bool, evidenceErr bool) {
	filename := TransferFilename(sourceHost, transferID)
	for _, box := range []string{"new", "cur"} {
		present, errFlag := lookupInBox(root, localAgent, box, filename)
		if errFlag {
			return false, true
		}
		if present {
			// The retained artifact is the delivery evidence. Its bytes were
			// verified digest-matching at apply time; a present same-name
			// artifact with unknown bytes cannot be distinguished here, so
			// read and compare to fail closed rather than trust the name.
			data, err := root.ReadRegularNoFollow(filepath.Join("agents", localAgent, "inbox", box, filename))
			if err != nil {
				return false, true
			}
			if Digest(data) == wantDigest {
				return true, false
			}
			// Same name, different bytes: not evidence of THIS transfer.
			continue
		}
	}
	return scanDLQForOriginal(root, localAgent, filename, wantDigest)
}

// ApplyOutcome is the caller-facing result of one ledgered apply attempt.
type ApplyOutcome struct {
	State    LedgerState
	Replayed bool
	Path     string
	// Reason carries the rejection reason when State is rejected.
	Reason string
	// Evidence describes what was examined when the state is uncertain.
	Evidence string
}

// ApplyWithLedger runs the owning-layer apply state machine for one envelope:
//
//	append prepared -> ApplyEnvelope -> append committed | rejected
//
// A crash between prepared and committed leaves a prepared record; recovery
// (a later call for the same transfer) resolves it by publication evidence,
// applying only when no evidence of prior delivery exists AND the prepared
// record is known not to have been applied — which is never knowable, so a
// recovered prepared record becomes uncertain unless evidence is found:
// re-applying after an unknown-history drain could duplicate delivery.
func ApplyWithLedger(ledger *TransferLedger, root *fsq.DeliveryRoot, localHost, localAgent string, env Envelope) (ApplyOutcome, error) {
	if err := ValidateEnvelope(env); err != nil {
		return ApplyOutcome{}, err
	}
	destHost, destAgent, err := ParseAlias(env.DestAlias)
	if err != nil {
		return ApplyOutcome{}, err
	}
	if destHost != localHost {
		return ApplyOutcome{}, fmt.Errorf("bridge dest_alias host %q is not local host %q", destHost, localHost)
	}
	if destAgent != localAgent {
		return ApplyOutcome{}, fmt.Errorf("bridge dest_alias agent %q is not local agent %q", destAgent, localAgent)
	}
	if ledger == nil {
		return ApplyOutcome{}, fmt.Errorf("transfer ledger is required")
	}

	var outcome ApplyOutcome
	lockErr := ledger.withTransferLock(env.SourceHost, env.TransferID, func() error {
		rec, torn, err := ledger.effectiveRecord(env.SourceHost, env.TransferID)
		if err != nil {
			return err
		}
		if torn {
			outcome = ApplyOutcome{
				State:    LedgerUncertain,
				Evidence: "torn ledger state preserved",
			}
			return nil
		}

		switch {
		case rec == nil:
			// Fresh transfer: append the intent record, then apply.
			if err := ledger.appendRecord(ledgerRecord{
				Version:       ledgerSchemaVersion,
				State:         LedgerPrepared,
				SourceHost:    env.SourceHost,
				TransferID:    env.TransferID,
				PayloadSHA256: env.PayloadSHA256,
			}); err != nil {
				return fmt.Errorf("record prepared: %w", err)
			}
			return ledger.applyAfterPrepared(root, localAgent, env, &outcome)

		case rec.State == LedgerCommitted:
			// Verified commit already recorded. A replay with the same
			// digest is idempotent; a different digest under the same key
			// is a conflict.
			if !strings.EqualFold(rec.PayloadSHA256, env.PayloadSHA256) {
				return ledger.rejectConflict(env, &outcome)
			}
			outcome = ApplyOutcome{State: LedgerCommitted, Replayed: true, Path: rec.CommittedPath}
			return nil

		case rec.State == LedgerRejected:
			// Terminal. Same digest replays the rejection; different digest
			// is still a conflict for that key.
			outcome = ApplyOutcome{State: LedgerRejected, Reason: rec.Reason}
			if !strings.EqualFold(rec.PayloadSHA256, env.PayloadSHA256) {
				outcome.Reason = ledgerReasonConflict
			}
			return nil

		case rec.State == LedgerPrepared:
			// Crash history A or B: the intent exists but the commit record
			// does not. Consult publication evidence.
			found, evidenceErr := publicationEvidence(root, localAgent, env.SourceHost, env.TransferID, env.PayloadSHA256)
			if evidenceErr {
				outcome = ApplyOutcome{
					State:    LedgerUncertain,
					Evidence: "publication evidence unreadable",
				}
				return nil
			}
			if found {
				if !strings.EqualFold(rec.PayloadSHA256, env.PayloadSHA256) {
					return ledger.rejectConflict(env, &outcome)
				}
				// Promote on verified evidence.
				if err := ledger.appendRecord(ledgerRecord{
					Version:       ledgerSchemaVersion,
					State:         LedgerCommitted,
					SourceHost:    env.SourceHost,
					TransferID:    env.TransferID,
					PayloadSHA256: env.PayloadSHA256,
				}); err != nil {
					return fmt.Errorf("record committed: %w", err)
				}
				outcome = ApplyOutcome{State: LedgerCommitted, Replayed: true}
				return nil
			}
			// No evidence: the prepared intent may or may not have been
			// applied and drained. Unknown history. Refuse — and record the
			// refusal durably so status/doctor surfaces it and later calls
			// see the same disposition (append-only: history is preserved).
			if err := ledger.appendRecord(ledgerRecord{
				Version:       ledgerSchemaVersion,
				State:         LedgerUncertain,
				SourceHost:    env.SourceHost,
				TransferID:    env.TransferID,
				PayloadSHA256: env.PayloadSHA256,
				Reason:        "prepared without publication evidence",
			}); err != nil {
				return fmt.Errorf("record uncertain: %w", err)
			}
			outcome = ApplyOutcome{
				State:    LedgerUncertain,
				Evidence: "prepared without publication evidence",
			}
			return nil

		case rec.State == LedgerUncertain:
			// Re-check evidence: it may have appeared since (e.g. the
			// consumer moved the artifact, or an operator DLQ'd a parse
			// failure). Otherwise remain refused.
			found, evidenceErr := publicationEvidence(root, localAgent, env.SourceHost, env.TransferID, env.PayloadSHA256)
			if evidenceErr {
				outcome = ApplyOutcome{State: LedgerUncertain, Evidence: "publication evidence unreadable"}
				return nil
			}
			if found && strings.EqualFold(rec.PayloadSHA256, env.PayloadSHA256) {
				if err := ledger.appendRecord(ledgerRecord{
					Version:       ledgerSchemaVersion,
					State:         LedgerCommitted,
					SourceHost:    env.SourceHost,
					TransferID:    env.TransferID,
					PayloadSHA256: env.PayloadSHA256,
				}); err != nil {
					return fmt.Errorf("record committed: %w", err)
				}
				outcome = ApplyOutcome{State: LedgerCommitted, Replayed: true}
				return nil
			}
			outcome = ApplyOutcome{State: LedgerUncertain, Evidence: rec.Reason}
			if outcome.Evidence == "" {
				outcome.Evidence = "previously uncertain; no publication evidence"
			}
			return nil
		}
		return fmt.Errorf("unhandled ledger state %q", rec.State)
	})
	if lockErr != nil {
		return ApplyOutcome{}, lockErr
	}
	return outcome, nil
}

// applyAfterPrepared runs ApplyEnvelope for a fresh prepared record and
// records the terminal state. ApplyEnvelope's own idempotency (same-digest
// replay returns Replayed=true) and conflict translation (os.ErrExist,
// different digest) drive committed vs rejected.
func (l *TransferLedger) applyAfterPrepared(root *fsq.DeliveryRoot, localAgent string, env Envelope, outcome *ApplyOutcome) error {
	applyResult, err := ApplyEnvelope(root, hostOfAlias(env.DestAlias), localAgent, env)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return l.rejectConflict(env, outcome)
		}
		return err
	}
	if err := l.appendRecord(ledgerRecord{
		Version:       ledgerSchemaVersion,
		State:         LedgerCommitted,
		SourceHost:    env.SourceHost,
		TransferID:    env.TransferID,
		PayloadSHA256: env.PayloadSHA256,
		CommittedPath: applyResult.Path,
	}); err != nil {
		return fmt.Errorf("record committed: %w", err)
	}
	*outcome = ApplyOutcome{State: LedgerCommitted, Replayed: applyResult.Replayed, Path: applyResult.Path}
	return nil
}

func (l *TransferLedger) rejectConflict(env Envelope, outcome *ApplyOutcome) error {
	if err := l.appendRecord(ledgerRecord{
		Version:       ledgerSchemaVersion,
		State:         LedgerRejected,
		SourceHost:    env.SourceHost,
		TransferID:    env.TransferID,
		PayloadSHA256: env.PayloadSHA256,
		Reason:        ledgerReasonConflict,
	}); err != nil {
		return fmt.Errorf("record rejected: %w", err)
	}
	*outcome = ApplyOutcome{State: LedgerRejected, Reason: ledgerReasonConflict}
	return nil
}

// UncertainTransfers lists every ledger record currently in the uncertain
// state (with its evidence note) so status/doctor can surface them.
func (l *TransferLedger) UncertainTransfers() ([]LedgerRecord, error) {
	entries, err := l.root.ReadDir(l.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".jsonl") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	var out []LedgerRecord
	for _, name := range names {
		host, id, ok := splitLedgerName(name)
		if !ok {
			continue
		}
		rec, torn, err := l.effectiveRecord(host, id)
		if err != nil {
			return nil, err
		}
		switch {
		case torn:
			out = append(out, LedgerRecord{
				State:      LedgerUncertain,
				SourceHost: host,
				TransferID: id,
				Reason:     "torn ledger state preserved",
			})
		case rec != nil && rec.State == LedgerUncertain:
			out = append(out, LedgerRecord{
				State:         rec.State,
				SourceHost:    rec.SourceHost,
				TransferID:    rec.TransferID,
				PayloadSHA256: rec.PayloadSHA256,
				Reason:        rec.Reason,
				RecordedAt:    rec.RecordedAt,
			})
		}
	}
	return out, nil
}

func splitLedgerName(name string) (sourceHost, transferID string, ok bool) {
	base := strings.TrimSuffix(name, ".jsonl")
	// key = sourceHost + "-" + transferID; transferID is fixed-length base32.
	idx := strings.LastIndex(base, "-")
	if idx <= 0 || idx == len(base)-1 {
		return "", "", false
	}
	return base[:idx], base[idx+1:], true
}

func hostOfAlias(alias string) string {
	host, _, err := ParseAlias(alias)
	if err != nil {
		return ""
	}
	return host
}
