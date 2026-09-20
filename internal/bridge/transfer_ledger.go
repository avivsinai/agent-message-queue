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
	// LedgerPublishedDurabilityUnknown records the post-publication
	// uncertainty class (codex r2-r2 finding 1): the rename into inbox/new
	// SUCCEEDED (the artifact is visible) but the destination directory
	// sync failed — CommittedDurabilityError. The publication is a fact and
	// must never be re-applied (that duplicates); the DURABILITY of that
	// publication is unproven, so the success receipt and the ACK are
	// withheld until the destination directory sync is verified repaired.
	// This is a distinct third class: not a proven non-delivery (retryable),
	// not durable completion (committed), and not unknown history
	// (uncertain) — the artifact's location is KNOWN.
	LedgerPublishedDurabilityUnknown LedgerState = "published_durability_unknown"
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
	// Retryable marks a rejected record whose rejection is a PROVEN
	// non-delivery that a same-digest retry may re-apply (review-827-r2
	// P2-1: a discriminator that gates re-delivery is a typed field, not a
	// free-text reason prefix a future error-message edit could break).
	Retryable     bool   `json:"retryable,omitempty"`
	Reason        string `json:"reason,omitempty"`
	CommittedPath string `json:"committed_path,omitempty"`
	RecordedAt    string `json:"recorded_at"`
}

// LedgerRecord is the caller-facing view of one transfer's ledger entry.
type LedgerRecord struct {
	State         LedgerState
	SourceHost    string
	TransferID    string
	PayloadSHA256 string
	Retryable     bool
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
// capability; the write and its durability sync are one unit —
// AppendLedgerLine syncs on file creation, on an empty crash orphan, and on a
// recovered earlier failed sync (ensureLedgerDirDurability), so durability is
// never inferred from the created flag (review-827-r2 P2-4: this one helper
// used to exist twice under two names claiming different guarantees).
func (l *TransferLedger) appendRecord(rec ledgerRecord) error {
	data, err := ledgerLine(rec)
	if err != nil {
		return err
	}
	_, err = l.root.AppendLedgerLine(l.dir, ledgerRecordName(rec.SourceHost, rec.TransferID), data)
	return err
}

// appendRecordFramed writes a recovered record after a torn read: it first
// appends a bare newline to TERMINATE whatever torn tail line precedes it,
// then the record on its own line (codex batch finding 3, review-827-r2).
// Appending JSON directly after a partial tail yields one concatenated line
// that readRecords discards wholesale — the promotion would be unreadable on
// the next reread. Each append is its own fsynced unit; the torn prefix stays
// torn and surfaces via the torn flag.
func (l *TransferLedger) appendRecordFramed(rec ledgerRecord) error {
	if _, err := l.root.AppendLedgerLine(l.dir, ledgerRecordName(rec.SourceHost, rec.TransferID), []byte("\n")); err != nil {
		return fmt.Errorf("frame torn tail: %w", err)
	}
	return l.appendRecord(rec)
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
	if err != nil {
		return nil, torn, err
	}
	if len(records) == 0 {
		return nil, torn, nil
	}
	// The last valid record survives a torn tail: the intact prefix stays
	// authoritative (review-827-r1 P1c — an unparseable tail is a torn
	// append over usable history, not a poison of it).
	return &records[len(records)-1], torn, nil
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

// scanDLQForOriginalPath scans dlq/new and dlq/cur for envelopes whose
// OriginalFile equals the transfer filename and whose ORIGINAL body bytes
// (returned by ReadDLQEnvelope — never the wrapper) hash to wantDigest. It
// also reports the root-relative path of the matching DLQ envelope (the
// delivery carrier for an evidence promotion).
// DLQ delivery creates a new id/filename and wraps the original bytes
// (internal/fsq/dlq.go moveInboxMessageToDLQ), so the transfer filename is
// never the DLQ filename.
func scanDLQForOriginalPath(root *fsq.DeliveryRoot, agent, filename, wantDigest string) (found bool, path string, evidenceErr bool) {
	for _, box := range []string{"new", "cur"} {
		dir := filepath.Join("agents", agent, "dlq", box)
		entries, err := root.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, "", true
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
				return false, "", true
			}
			if envelope.OriginalFile != filename {
				continue
			}
			if Digest(original) == wantDigest {
				return true, path, false
			}
		}
	}
	return false, "", false
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

// publicationEvidencePath consults durable publication evidence independent
// of inbox/new. Evidence is conditional and fails closed:
//
//   - inbox/new and inbox/cur: deterministic transfer filename
//     (TransferFilename) looked up directly; a claim never replaces a
//     retained cur copy.
//   - DLQ: envelopes wrap the original bytes under a new id/filename, so
//     match on OriginalFile + original-content digest.
//
// Any read/stat error is treated as absence of proof (evidenceErr), never
// proof of absence. The returned path is the root-relative path of the
// artifact that carried the evidence, so the promoted committed record (and
// its receipt) can name the retained delivery (review-827-r1 P2-2:
// evidence-promoted commits must not lose committed_path).
func publicationEvidencePath(root *fsq.DeliveryRoot, localAgent, sourceHost, transferID, wantDigest string) (found bool, path string, evidenceErr bool) {
	filename := TransferFilename(sourceHost, transferID)
	for _, box := range []string{"new", "cur"} {
		rel := filepath.Join("agents", localAgent, "inbox", box, filename)
		present, errFlag := lookupInBox(root, localAgent, box, filename)
		if errFlag {
			return false, "", true
		}
		if present {
			// The retained artifact is the delivery evidence. Its bytes were
			// verified digest-matching at apply time; a present same-name
			// artifact with unknown bytes cannot be distinguished here, so
			// read and compare to fail closed rather than trust the name.
			data, err := root.ReadRegularNoFollow(rel)
			if err != nil {
				return false, "", true
			}
			if Digest(data) == wantDigest {
				return true, rel, false
			}
			// Same name, different bytes: not evidence of THIS transfer.
			continue
		}
	}
	found, dlqPath, errFlag := scanDLQForOriginalPath(root, localAgent, filename, wantDigest)
	if errFlag {
		return false, "", true
	}
	return found, dlqPath, false
}

// promoteViaEvidence is the single shared evidence-promotion path: every
// branch that turns retained publication evidence into a committed record
// goes through here. Its two operations, in order:
//
//  1. Carrier durability: the evidence's carrier directory is fsynced
//     before any record is written — a directory entry the carrier never
//     fsynced can vanish in a machine crash, so a commit recorded over an
//     unsynced carrier is a receipt for a delivery that may not survive.
//
//  2. Publication observed: when publication is observed but durability
//     cannot be verified, a durable published_durability_unknown record is
//     appended before the outcome returns. If that record itself fails to
//     land, the error is returned and no receipt or ACK flows; the last
//     durable state is whatever preceded this call — prepared (the retry
//     arm re-arms before promoting) or another non-retryable recovery
//     state, both of which recover without blind re-apply. If the record
//     lands, later calls re-enter at the published-unknown state, which
//     promotes only from digest-verified, durability-verified evidence —
//     never re-applies. A returned outcome struct is not persistent
//     state; only the ledger is.
//
// The torn flag selects framed appends: after a torn read every record
// written here must be separately readable (appendRecordFramed terminates
// the torn tail first).
func (l *TransferLedger) promoteViaEvidence(root *fsq.DeliveryRoot, sourceHost, transferID, payloadSHA256, evidencePath string, torn bool, outcome *ApplyOutcome) error {
	writeRec := func(rec ledgerRecord) error {
		if torn {
			return l.appendRecordFramed(rec)
		}
		return l.appendRecord(rec)
	}
	if syncErr := root.SyncDir(filepath.Dir(evidencePath)); syncErr != nil {
		if recErr := writeRec(ledgerRecord{
			Version:       ledgerSchemaVersion,
			State:         LedgerPublishedDurabilityUnknown,
			SourceHost:    sourceHost,
			TransferID:    transferID,
			PayloadSHA256: payloadSHA256,
			CommittedPath: evidencePath,
			Reason:        "evidence present but carrier durability unverified: " + syncErr.Error(),
		}); recErr != nil {
			// The durable record failed to land; fail closed — no receipt or
			// ACK flows. The state left behind is the caller's pre-call
			// recovery state (prepared, via re-arm-first), which recovers
			// without blind re-apply.
			return fmt.Errorf("record published_durability_unknown: %w", recErr)
		}
		*outcome = ApplyOutcome{
			State:    LedgerPublishedDurabilityUnknown,
			Replayed: true,
			Path:     evidencePath,
			Evidence: "evidence present but carrier durability unverified: " + syncErr.Error(),
		}
		return nil
	}
	rec := ledgerRecord{
		Version:       ledgerSchemaVersion,
		State:         LedgerCommitted,
		SourceHost:    sourceHost,
		TransferID:    transferID,
		PayloadSHA256: payloadSHA256,
		CommittedPath: evidencePath,
	}
	if err := writeRec(rec); err != nil {
		return fmt.Errorf("record committed: %w", err)
	}
	*outcome = ApplyOutcome{State: LedgerCommitted, Replayed: true, Path: evidencePath}
	return nil
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
			// Torn ledger read (review-827-r1 P1c). The append-only contract
			// tolerates a torn TAIL: an unparseable line after intact records
			// is a crash-truncated append, and the intact prefix stays
			// authoritative — rec is the last valid record. A torn read with
			// NO valid record, or with a torn MIDDLE line (recorded by
			// readRecords as torn regardless of position), is ambiguous
			// state: refuse rather than guess. When the last valid record is
			// itself terminal (committed, or a NON-retryable proven
			// non-delivery/conflict), it governs — a torn tail after a terminal
			// record cannot un-terminal it. Retryable rejections and
			// published_durability_unknown are NOT terminal (codex r2-r2
			// finding 3): the torn tail could have been the commit that
			// resolved them, so they resolve only from verified evidence or
			// stay uncertain — an apply is never authorized from ambiguous
			// torn history.
			if rec == nil {
				outcome = ApplyOutcome{
					State:    LedgerUncertain,
					Evidence: "torn ledger state preserved",
				}
				return nil
			}
			terminal := rec.State == LedgerCommitted || rec.State == LedgerPublishedDurabilityUnknown ||
				(rec.State == LedgerRejected && !rec.Retryable)
			if !terminal {
				// Non-terminal prefix (prepared/uncertain/retryable-rejected)
				// + torn tail: the prefix may predate the torn append, but the
				// torn tail could have been a terminal record. Fail closed on
				// the tail while keeping the digest binding: a same-digest
				// arrival still resolves via publication evidence below; a
				// conflicting digest is refused here.
				if !strings.EqualFold(rec.PayloadSHA256, env.PayloadSHA256) {
					outcome = ApplyOutcome{
						State:    LedgerUncertain,
						Evidence: "torn ledger state preserved",
					}
					return nil
				}
				found, evidencePath, evidenceErr := publicationEvidencePath(root, localAgent, env.SourceHost, env.TransferID, rec.PayloadSHA256)
				if evidenceErr {
					outcome = ApplyOutcome{
						State:    LedgerUncertain,
						Evidence: "publication evidence unreadable",
					}
					return nil
				}
				if found {
					// Shared promotion path (codex r3 finding): verify the
					// evidence carrier's durability before the commit append,
					// and append the record FRAMED so it is separately
					// readable after the torn tail it recovers over.
					return ledger.promoteViaEvidence(root, env.SourceHost, env.TransferID, rec.PayloadSHA256, evidencePath, true, &outcome)
				}
				outcome = ApplyOutcome{
					State:    LedgerUncertain,
					Evidence: "torn ledger tail over non-terminal record; no publication evidence",
				}
				return nil
			}
			// Terminal prefix governs (torn tail ignored for disposition,
			// surfaced via UnresolvedTransfers torn flag).
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
			found, evidencePath, evidenceErr := publicationEvidencePath(root, localAgent, env.SourceHost, env.TransferID, env.PayloadSHA256)
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
				// re-applying; the retained artifact is the delivery. Shared
				// promotion path (codex r3 finding): carrier durability is
				// verified before the commit record is appended.
				if err := ledger.appendPrepared(env); err != nil {
					return err
				}
				return ledger.promoteViaEvidence(root, env.SourceHost, env.TransferID, env.PayloadSHA256, evidencePath, false, &outcome)
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

		case rec.State == LedgerRejected && rec.Retryable && strings.EqualFold(rec.PayloadSHA256, env.PayloadSHA256):
			// Retry arm. Order is the invariant: under the held per-transfer
			// lock, a fresh durable prepared intent is appended before any
			// evidence lookup or promotion. The old rejected(retryable) record
			// must not remain the effective state while this invocation
			// inspects the world — if a later append fails or the process
			// crashes, the ledger's last durable state is prepared, which
			// recovers without blind re-apply. If the intent append fails, no
			// recovery or apply work happens this invocation.
			if err := ledger.appendPrepared(env); err != nil {
				outcome = ApplyOutcome{
					State:  LedgerRejected,
					Reason: "apply failed (retryable): re-arming prepared intent failed: " + err.Error(),
				}
				return nil
			}
			// The intent is durable. Evidence may have appeared since the
			// failure (consumer drain, operator repair): a digest match
			// promotes without re-applying; absence authorizes the apply via
			// the standard prepared→terminal path.
			found, evidencePath, evidenceErr := publicationEvidencePath(root, localAgent, env.SourceHost, env.TransferID, rec.PayloadSHA256)
			if evidenceErr {
				outcome = ApplyOutcome{State: LedgerUncertain, Evidence: "publication evidence unreadable"}
				return nil
			}
			if found {
				// Shared promotion path: verifies the carrier's durability
				// before committing — an unsynced directory entry is not a
				// delivery the ledger may call committed.
				return ledger.promoteViaEvidence(root, env.SourceHost, env.TransferID, rec.PayloadSHA256, evidencePath, torn, &outcome)
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
			found, evidencePath, evidenceErr := publicationEvidencePath(root, localAgent, env.SourceHost, env.TransferID, rec.PayloadSHA256)
			if evidenceErr {
				outcome = ApplyOutcome{
					State:    LedgerUncertain,
					Evidence: "publication evidence unreadable",
				}
				return nil
			}
			if found {
				// Promote on verified evidence via the shared promotion path
				// — it verifies the durability of the evidence's actual
				// carrier (codex r2-r3 finding 1) before the commit append:
				// a crash after publication but before the commit append must
				// not promote from an unsynced directory entry.
				return ledger.promoteViaEvidence(root, env.SourceHost, env.TransferID, rec.PayloadSHA256, evidencePath, torn, &outcome)
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

		case rec.State == LedgerPublishedDurabilityUnknown:
			// Codex r2-r2 finding 1, tightened per codex r2-r3 finding 1: the
			// artifact WAS published at rec.CommittedPath, but its durability
			// is unproven — a machine crash can lose the unsynced rename, and
			// fsyncing an empty inbox/new proves nothing about a carrier the
			// consumer already moved. Promotion therefore requires
			// digest-VERIFIED evidence (the artifact actually present with
			// exactly the prepared digest) and verifies the durability of
			// that evidence's actual carrier directory — never a blanket
			// inbox/new sync, never the recorded path alone. Without
			// evidence the publication's survival is unprovable: stay
			// uncertain (never re-apply an ambiguous publication). The
			// rendezvous redelivers either way; no re-apply happens here.
			if !strings.EqualFold(rec.PayloadSHA256, env.PayloadSHA256) {
				outcome = ApplyOutcome{State: LedgerUncertain, Reason: ledgerReasonConflict, Evidence: "published digest binding differs from arrival"}
				return nil
			}
			found, evidencePath, evidenceErr := publicationEvidencePath(root, localAgent, env.SourceHost, env.TransferID, rec.PayloadSHA256)
			if evidenceErr {
				outcome = ApplyOutcome{State: LedgerUncertain, Evidence: "publication evidence unreadable"}
				return nil
			}
			if !found {
				outcome = ApplyOutcome{
					State:    LedgerUncertain,
					Evidence: "published but no retained evidence; delivery survival unprovable after crash",
				}
				return nil
			}
			// Shared promotion path, reuse not reimplementation: same carrier
			// verification, same durable publication-observed record, same
			// commit append as every other evidence promotion. The published
			// state can be torn (its append may be the torn tail), so torn is
			// forwarded for framing.
			return ledger.promoteViaEvidence(root, env.SourceHost, env.TransferID, rec.PayloadSHA256, evidencePath, torn, &outcome)

		case rec.State == LedgerUncertain:
			// Re-check evidence: it may have appeared since (e.g. the
			// consumer moved the artifact, or an operator DLQ'd a parse
			// failure). Otherwise remain refused.
			found, evidencePath, evidenceErr := publicationEvidencePath(root, localAgent, env.SourceHost, env.TransferID, env.PayloadSHA256)
			if evidenceErr {
				outcome = ApplyOutcome{State: LedgerUncertain, Evidence: "publication evidence unreadable"}
				return nil
			}
			if found && strings.EqualFold(rec.PayloadSHA256, env.PayloadSHA256) {
				// Shared promotion path (codex r3 finding): verify the
				// carrier's durability before the commit append; the uncertain
				// state may itself be torn, so frame the append when torn.
				return ledger.promoteViaEvidence(root, env.SourceHost, env.TransferID, rec.PayloadSHA256, evidencePath, torn, &outcome)
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
// success the committed record carries that digest.
//
// Outcome recording (review-827-r1 P0, amended per review-827-r2 P0):
//   - os.ErrExist on a fresh key means a same-name artifact with different
//     bytes exists — a conflict observed in the outcome. The arrival is a
//     proven NON-delivery, so it is recorded as terminal rejected
//     (Reason=transfer_conflict) — not left as a bare prepared that a later
//     reader must re-derive (previously: rejected once, then uncertain on
//     every retry — two answers for one set of facts).
//   - *fsq.CommittedDurabilityError means the rename into inbox/new
//     SUCCEEDED and only the destination directory sync failed: the
//     publication is a FACT and is never re-applied (re-applying would
//     duplicate once the consumer drains new → cur), but its durability is
//     UNPROVEN — the ledger append below syncs bridge/transfer-ledger
//     ancestors, NOT agents/<agent>/inbox/new, so it does not repair the
//     destination. It is recorded as published_durability_unknown with the
//     error's FinalPath: the success receipt and the ACK are WITHHELD and
//     the transfer is surfaced via UnresolvedTransfers until a later call
//     verifies the destination directory sync (DeliverToExistingInbox
//     returning success or a digest-matching artifact with a clean sync).
//   - os.ErrExist on a fresh key is a same-name artifact whose bytes could
//     not be PROVEN different here: resolvePublishCollision wraps the
//     collision ErrExist when the destination read fails, and a proven-
//     different read is a conflict. A proven-different read records
//     terminal rejected (transfer_conflict); an UNPROVEN collision (read
//     failure under ErrExist) is post-publication ambiguity — recorded as
//     uncertain, never as retryable (re-apply could duplicate the visible
//     artifact) and never as a definitive conflict.
//   - Any other in-process apply failure is a PRE-publication failure: the
//     staging/temp-file path failed before any rename (the only
//     publish-then-error mode is CommittedDurabilityError, handled above).
//     It is recorded as a retryable rejected record (typed Retryable=true)
//     carrying the failure reason, so the message stays recoverable and the
//     retry branch consults publication evidence before re-applying (below)
//     — the same failsafe every other recovery branch uses.
func (l *TransferLedger) applyAfterPrepared(root *fsq.DeliveryRoot, localAgent string, env Envelope, outcome *ApplyOutcome) error {
	applyResult, err := ApplyEnvelope(root, hostOfAlias(env.DestAlias), localAgent, env)
	if err != nil {
		var committed *fsq.CommittedDurabilityError
		if errors.As(err, &committed) {
			// Published but durability unknown: a distinct persistent class.
			// The courier withholds the receipt and the ACK for this state;
			// the next apply re-verifies destination durability and promotes
			// to committed WITHOUT re-applying.
			if recErr := l.appendRecord(ledgerRecord{
				Version:       ledgerSchemaVersion,
				State:         LedgerPublishedDurabilityUnknown,
				SourceHost:    env.SourceHost,
				TransferID:    env.TransferID,
				PayloadSHA256: env.PayloadSHA256,
				CommittedPath: committed.FinalPath,
				Reason:        "published at " + committed.FinalPath + "; destination durability unverified: " + committed.Err.Error(),
			}); recErr != nil {
				return fmt.Errorf("record published_durability_unknown: %w", recErr)
			}
			*outcome = ApplyOutcome{State: LedgerPublishedDurabilityUnknown, Path: committed.FinalPath}
			return nil
		}
		if errors.Is(err, ErrCollisionUnproven) {
			// Codex r2-r3 finding 2: check the sentinel INDEPENDENTLY of
			// os.ErrExist — the wrap nests it under the collision error, and
			// classification must not depend on the outer type surviving.
			// Collision bytes unreadable: neither proven absent nor proven
			// conflict. Post-publication ambiguity → uncertain. A later
			// retry re-checks evidence and the collision read before any
			// re-apply.
			if recErr := l.appendRecord(ledgerRecord{
				Version:       ledgerSchemaVersion,
				State:         LedgerUncertain,
				SourceHost:    env.SourceHost,
				TransferID:    env.TransferID,
				PayloadSHA256: env.PayloadSHA256,
				Reason:        "collision bytes unreadable: " + err.Error(),
			}); recErr != nil {
				return fmt.Errorf("record uncertain: %w", recErr)
			}
			*outcome = ApplyOutcome{State: LedgerUncertain, Evidence: "collision bytes unreadable; conflict unproven"}
			return nil
		}
		if errors.Is(err, os.ErrExist) {
			// Proven different bytes on a fresh key: a conflict observed in
			// the outcome. The arrival is a proven NON-delivery, recorded as
			// terminal rejected (Reason=transfer_conflict).
			if recErr := l.appendRecord(ledgerRecord{
				Version:       ledgerSchemaVersion,
				State:         LedgerRejected,
				SourceHost:    env.SourceHost,
				TransferID:    env.TransferID,
				PayloadSHA256: env.PayloadSHA256,
				Reason:        ledgerReasonConflict,
			}); recErr != nil {
				return fmt.Errorf("record rejected: %w", recErr)
			}
			*outcome = ApplyOutcome{State: LedgerRejected, Reason: ledgerReasonConflict}
			return nil
		}
		// Proven non-delivery: record it as retryable rejected (typed flag).
		// A retry with the same digest consults publication evidence FIRST
		// (review-827-r2 P0) — the consumer may have drained the artifact
		// between the failure and the retry — then re-applies only if no
		// evidence exists. The ledger keeps the full history.
		if recErr := l.appendRecord(ledgerRecord{
			Version:       ledgerSchemaVersion,
			State:         LedgerRejected,
			SourceHost:    env.SourceHost,
			TransferID:    env.TransferID,
			PayloadSHA256: env.PayloadSHA256,
			Retryable:     true,
			Reason:        "apply failed (retryable): " + err.Error(),
		}); recErr != nil {
			return fmt.Errorf("record rejected: %w", recErr)
		}
		*outcome = ApplyOutcome{State: LedgerRejected, Reason: "apply failed (retryable): " + err.Error()}
		return nil
	}
	if applyResult.DurabilityIndeterminate {
		// ApplyEnvelope already converted CommittedDurabilityError into a
		// committed-but-unverified result (its repo-wide contract). The
		// ledger records the distinct published_durability_unknown class:
		// published, receipt/ACK withheld until durability re-verified.
		if recErr := l.appendRecord(ledgerRecord{
			Version:       ledgerSchemaVersion,
			State:         LedgerPublishedDurabilityUnknown,
			SourceHost:    env.SourceHost,
			TransferID:    env.TransferID,
			PayloadSHA256: env.PayloadSHA256,
			CommittedPath: applyResult.Path,
			Reason:        "published at " + applyResult.Path + "; destination durability unverified",
		}); recErr != nil {
			return fmt.Errorf("record published_durability_unknown: %w", recErr)
		}
		*outcome = ApplyOutcome{State: LedgerPublishedDurabilityUnknown, Path: applyResult.Path}
		return nil
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
	if err := l.appendRecord(ledgerRecord{
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
// still owes action: prepared, uncertain, torn records, AND rejected records
// with the typed Retryable flag (review-827-r2 P2-2 — a stuck retryable
// transfer is a message still owed to its recipient, waiting for someone to
// retry; it must not read as terminal). This is the production diagnostic
// surface; the courier's run result wires it into operator-visible output
// (cmd/amq-bridge/main.go RunResult.Diagnostics).
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
		case rec != nil && (rec.State == LedgerUncertain || rec.State == LedgerPrepared || rec.State == LedgerPublishedDurabilityUnknown || (rec.State == LedgerRejected && rec.Retryable)):
			out = append(out, LedgerRecord{
				State:         rec.State,
				SourceHost:    rec.SourceHost,
				TransferID:    rec.TransferID,
				PayloadSHA256: rec.PayloadSHA256,
				Retryable:     rec.Retryable,
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
