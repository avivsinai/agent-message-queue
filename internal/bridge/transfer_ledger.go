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
// ledger directory name without narrowing the wire alias contract: each
// validated component is percent-escaped for the characters that are not
// path-safe, then joined with a single underscore. Both components are
// already constrained by ParseAlias (lowercase/digits/underscore/dash, up to
// 63 bytes each), so the encoding is identity for all currently valid
// aliases and can only ever grow bounded (max 2*63*3+1 bytes).
func ledgerSessionName(receiveAlias string) (string, error) {
	host, agent, err := ParseAlias(receiveAlias)
	if err != nil {
		return "", err
	}
	return escapeLedgerComponent(host) + "_" + escapeLedgerComponent(agent), nil
}

// escapeLedgerComponent percent-escapes any byte outside the bridge
// identifier alphabet (lowercase, digits, '_', '-'), which ParseAlias
// already enforces; this keeps the on-disk name stable and path-safe even
// if the alias grammar ever widens.
func escapeLedgerComponent(component string) string {
	var b strings.Builder
	for i := 0; i < len(component); i++ {
		c := component[i]
		if isLowerASCII(c) || isASCIIDigit(c) || c == '_' || c == '-' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
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
	// LedgerReasonConflict marks a conflict: a same-key arrival whose
	// digest differs from the key's immutable binding.
	LedgerReasonConflict = "transfer_conflict"
	// ledgerReasonConflict is the internal alias.
	ledgerReasonConflict = LedgerReasonConflict
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
// capability; the write and its durability sync are one unit. On first
// creation of the ledger file its directory chain is synced too (fix: a
// machine crash must not preserve the message while losing the ledger name —
// see section 7.5's directory-sync requirement).
func (l *TransferLedger) appendRecord(rec ledgerRecord) error {
	data, err := ledgerLine(rec)
	if err != nil {
		return err
	}
	_, err = l.root.AppendLedgerLine(l.dir, ledgerRecordName(rec.SourceHost, rec.TransferID), data)
	return err
}

// ledgerLine serializes one record as a newline-terminated JSON line.
func ledgerLine(rec ledgerRecord) ([]byte, error) {
	if rec.RecordedAt == "" {
		rec.RecordedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// appendRecordFirstWrite appends a record and guarantees the ledger file's
// directory chain is durable before returning: AppendLedgerLine owns the
// guarantee (it syncs on file creation, on an empty crash orphan, and on a
// recovered earlier failed sync — see ensureLedgerDirDurability). This kept
// name is what the prepared-intent recovery depends on after a machine
// crash (section 7.5 directory-sync requirement).
func (l *TransferLedger) appendRecordFirstWrite(rec ledgerRecord) error {
	data, err := ledgerLine(rec)
	if err != nil {
		return err
	}
	_, err = l.root.AppendLedgerLine(l.dir, ledgerRecordName(rec.SourceHost, rec.TransferID), data)
	return err
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

// scanDLQForOriginalFile reports whether any DLQ envelope wraps an original
// named filename, regardless of content digest (the digest-agnostic twin of
// scanDLQForOriginal, used to detect that a transfer key's filename slot is
// occupied by SOME payload before binding a fresh arrival).
func scanDLQForOriginalFile(root *fsq.DeliveryRoot, agent, filename string) (found bool, evidenceErr bool) {
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
			envelope, _, err := fsq.ReadDLQEnvelope(root, filepath.Join(dir, entry.Name()))
			if err != nil {
				return false, true
			}
			if envelope.OriginalFile == filename {
				return true, false
			}
		}
	}
	return false, false
}

// filenameSlotOccupied reports whether the deterministic transfer filename
// is currently retained anywhere in the publication surface — inbox/new,
// inbox/cur, or as a DLQ original — regardless of which payload's bytes it
// holds. Digest-specific delivery proof is publicationEvidence's job; this
// occupancy check is what keeps a fresh arrival from binding a key whose
// filename slot already belongs to a different (possibly pre-ledger)
// payload.
func filenameSlotOccupied(root *fsq.DeliveryRoot, localAgent, sourceHost, transferID string) (occupied bool, evidenceErr bool) {
	filename := TransferFilename(sourceHost, transferID)
	for _, box := range []string{"new", "cur"} {
		present, errFlag := lookupInBox(root, localAgent, box, filename)
		if errFlag {
			return false, true
		}
		if present {
			return true, false
		}
	}
	return scanDLQForOriginalFile(root, localAgent, filename)
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
			// No ledger history for this key. Durable publication evidence may
			// still exist from BEFORE the ledger did (pre-ledger artifact in
			// new, a consumer-drained cur copy, or a DLQ envelope wrapping the
			// original bytes). Inspect it BEFORE binding the first digest and
			// before applying: without this, a replay of a drained pre-ledger
			// transfer re-applies (duplicate), and a conflicting first arrival
			// would bind the key away from the legitimate winner's evidence.
			found, evidenceErr := publicationEvidence(root, localAgent, env.SourceHost, env.TransferID, env.PayloadSHA256)
			if evidenceErr {
				outcome = ApplyOutcome{
					State:    LedgerUncertain,
					Evidence: "publication evidence unreadable",
				}
				return nil
			}
			if found {
				// The artifact is already delivered (retained new/cur or DLQ
				// evidence for exactly these bytes). Bind and commit without
				// re-applying; the retained artifact is the delivery.
				if err := ledger.appendPrepared(env); err != nil {
					return err
				}
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
			// No delivery evidence for THIS payload — but the key's filename
			// slot may still be occupied by a DIFFERENT payload (a pre-ledger
			// winner in new/cur/DLQ). Occupancy beats binding: the first
			// digest bound to the key must be the winner's, so refuse this
			// losing copy unambiguously without binding it, applying it, or
			// touching the winner's evidence. The key stays unbound so the
			// legitimate payload's own replay still recovers.
			occupied, occErr := filenameSlotOccupied(root, localAgent, env.SourceHost, env.TransferID)
			if occErr {
				outcome = ApplyOutcome{
					State:    LedgerUncertain,
					Evidence: "publication evidence unreadable",
				}
				return nil
			}
			if occupied {
				outcome = ApplyOutcome{
					State:    LedgerRejected,
					Reason:   ledgerReasonConflict,
					Evidence: "retained artifact for this transfer key belongs to a different payload",
				}
				return nil
			}
			// Fresh transfer: append the intent record durably (file AND
			// directory chain) before applying. The intent binds the FIRST
			// digest seen for this key; that binding is immutable for the
			// life of the key.
			if err := ledger.appendPrepared(env); err != nil {
				return err
			}
			return ledger.applyAfterPrepared(root, localAgent, env, &outcome)

		case rec.State == LedgerCommitted:
			// Verified commit already recorded. A replay with the same
			// digest is idempotent. A different digest under the same key
			// is a conflict for THAT received copy: the committed result of
			// the original binding is immutable and must not be replaced.
			// The stored winner stays committed; the received loser is
			// reported unambiguously as rejected with the conflict reason
			// (callers check Reason after State, but the loser must not be
			// readable as a success state with a hidden refusal flag).
			if !strings.EqualFold(rec.PayloadSHA256, env.PayloadSHA256) {
				outcome = ApplyOutcome{State: LedgerRejected, Replayed: true, Path: rec.CommittedPath, Reason: ledgerReasonConflict}
				return nil
			}
			outcome = ApplyOutcome{State: LedgerCommitted, Replayed: true, Path: rec.CommittedPath}
			return nil

		case rec.State == LedgerRejected:
			// Terminal for the bound digest. A different digest under the
			// same key is a conflict observed in the outcome; the terminal
			// rejection of the original binding is immutable.
			outcome = ApplyOutcome{State: LedgerRejected, Reason: rec.Reason}
			if !strings.EqualFold(rec.PayloadSHA256, env.PayloadSHA256) {
				outcome.Reason = ledgerReasonConflict
			}
			return nil

		case rec.State == LedgerPrepared:
			// Crash history A or B: the intent exists but the commit record
			// does not. The PREPARED digest governs recovery; the key's
			// binding is immutable. An arrival with a different digest is a
			// conflict observed in the outcome; it never replaces the
			// binding and never blocks the original transfer's recovery.
			if !strings.EqualFold(rec.PayloadSHA256, env.PayloadSHA256) {
				outcome = ApplyOutcome{State: LedgerUncertain, Reason: ledgerReasonConflict, Evidence: "prepared digest binding differs from arrival"}
				return nil
			}
			found, evidenceErr := publicationEvidence(root, localAgent, env.SourceHost, env.TransferID, rec.PayloadSHA256)
			if evidenceErr {
				outcome = ApplyOutcome{
					State:    LedgerUncertain,
					Evidence: "publication evidence unreadable",
				}
				return nil
			}
			if found {
				// Promote on verified evidence.
				if err := ledger.appendRecord(ledgerRecord{
					Version:       ledgerSchemaVersion,
					State:         LedgerCommitted,
					SourceHost:    env.SourceHost,
					TransferID:    env.TransferID,
					PayloadSHA256: rec.PayloadSHA256,
				}); err != nil {
					return fmt.Errorf("record committed: %w", err)
				}
				outcome = ApplyOutcome{State: LedgerCommitted, Replayed: true}
				return nil
			}
			// No evidence: the prepared intent may or may not have been
			// applied and drained. Unknown history. Refuse — and record the
			// refusal durably so the diagnostic surface reports it and later
			// calls see the same disposition (append-only: history preserved).
			if err := ledger.appendRecord(ledgerRecord{
				Version:       ledgerSchemaVersion,
				State:         LedgerUncertain,
				SourceHost:    env.SourceHost,
				TransferID:    env.TransferID,
				PayloadSHA256: rec.PayloadSHA256,
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
					PayloadSHA256: rec.PayloadSHA256,
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
// records the terminal state. The prepared digest binding governs: on
// success the committed record carries that digest. os.ErrExist on a fresh
// key means a same-name artifact with different bytes exists — a conflict
// observed in the outcome (Reason=transfer_conflict); NOTHING is appended,
// because the key has no terminal disposition of its own yet and the
// arriving copy must not overwrite whatever winning artifact exists.
func (l *TransferLedger) applyAfterPrepared(root *fsq.DeliveryRoot, localAgent string, env Envelope, outcome *ApplyOutcome) error {
	applyResult, err := ApplyEnvelope(root, hostOfAlias(env.DestAlias), localAgent, env)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			*outcome = ApplyOutcome{State: LedgerRejected, Reason: ledgerReasonConflict}
			return nil
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

// appendPrepared durably appends the intent record, syncing the file's
// directory chain on first creation so the prepared name survives a machine
// crash even if the later message publication also survives (fix: intent
// must be durable before apply — section 7.5 directory-sync requirement).
func (l *TransferLedger) appendPrepared(env Envelope) error {
	if err := l.appendRecordFirstWrite(ledgerRecord{
		Version:       ledgerSchemaVersion,
		State:         LedgerPrepared,
		SourceHost:    env.SourceHost,
		TransferID:    env.TransferID,
		PayloadSHA256: env.PayloadSHA256,
	}); err != nil {
		return fmt.Errorf("record prepared: %w", err)
	}
	return nil
}

// UnresolvedTransfers lists every ledger key whose latest durable disposition
// is not terminal (committed or rejected): prepared, uncertain, and torn
// records. This is the production diagnostic surface for status/doctor to
// report unknown-history transfers; courier integration wires it in.
func (l *TransferLedger) UnresolvedTransfers() ([]LedgerRecord, error) {
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
		case rec != nil && (rec.State == LedgerUncertain || rec.State == LedgerPrepared):
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

// UncertainTransfers is the legacy name of UnresolvedTransfers. It lists
// prepared, uncertain, and torn entries alike.
func (l *TransferLedger) UncertainTransfers() ([]LedgerRecord, error) {
	return l.UnresolvedTransfers()
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
