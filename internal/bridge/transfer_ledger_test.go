package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// rootRootDir recovers the delivery-root base directory from an opened
// capability: fsq agent paths are root-relative, so read a known file through
// the capability and compare against candidate dirs.
func rootRootDir(t *testing.T, root *fsq.DeliveryRoot) string {
	t.Helper()
	return ledgerTestBase
}

var ledgerTestBase string

func newLedgerTestRoot(t *testing.T) (*fsq.DeliveryRoot, *TransferLedger) {
	t.Helper()
	base := t.TempDir()
	ledgerTestBase = base
	t.Cleanup(func() { ledgerTestBase = "" })
	if err := fsq.EnsureAgentDirs(base, "claude"); err != nil {
		t.Fatal(err)
	}
	identity, err := fsq.SnapshotDeliveryRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	root, err := fsq.OpenDeliveryRoot(base, identity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	ledger, err := NewTransferLedger(root, "mac/claude")
	if err != nil {
		t.Fatal(err)
	}
	return root, ledger
}

func TestApplyWithLedgerHappyPath(t *testing.T) {
	root, ledger := newLedgerTestRoot(t)
	env := testEnvelope([]byte("hello"))

	outcome, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if outcome.State != LedgerCommitted || outcome.Replayed {
		t.Fatalf("state = %q replayed=%v, want committed fresh", outcome.State, outcome.Replayed)
	}

	// Replay of the same envelope is idempotent, no re-delivery side effect.
	outcome, err = ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if outcome.State != LedgerCommitted || !outcome.Replayed {
		t.Fatalf("replay state = %q replayed=%v, want committed replayed", outcome.State, outcome.Replayed)
	}
}

func TestApplyWithLedgerCommittedReplayDoesNotRepublish(t *testing.T) {
	root, ledger := newLedgerTestRoot(t)
	base := rootRootDir(t, root)
	env := testEnvelope([]byte("hello"))

	if _, err := ApplyWithLedger(ledger, root, "mac", "claude", env); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// Simulate crash history B evidence: consumer drained new -> cur.
	newPath := filepath.Join(fsq.AgentInboxNew(base, "claude"), TransferFilename(env.SourceHost, env.TransferID))
	curPath := filepath.Join(fsq.AgentInboxCur(base, "claude"), TransferFilename(env.SourceHost, env.TransferID))
	if err := os.Rename(newPath, curPath); err != nil {
		t.Fatal(err)
	}

	// Re-apply must NOT republish into new/: the ledger committed record +
	// cur evidence make the replay a no-op.
	outcome, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil {
		t.Fatalf("replay after drain: %v", err)
	}
	if outcome.State != LedgerCommitted || !outcome.Replayed {
		t.Fatalf("state = %q replayed=%v, want committed replayed", outcome.State, outcome.Replayed)
	}
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Fatalf("duplicate published into inbox/new: %v", err)
	}
	if _, err := os.Stat(curPath); err != nil {
		t.Fatalf("retained cur copy vanished: %v", err)
	}
}

func TestApplyWithLedgerPreparedWithoutEvidenceIsUncertain(t *testing.T) {
	root, ledger := newLedgerTestRoot(t)
	base := rootRootDir(t, root)
	env := testEnvelope([]byte("hello"))

	// Simulate crash history A/B with drained evidence: write a prepared
	// record directly (the crash ate the process before apply or before the
	// committed append), and remove any artifact.
	if err := ledger.appendRecord(ledgerRecord{
		Version:       ledgerSchemaVersion,
		State:         LedgerPrepared,
		SourceHost:    env.SourceHost,
		TransferID:    env.TransferID,
		PayloadSHA256: env.PayloadSHA256,
	}); err != nil {
		t.Fatal(err)
	}

	outcome, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if outcome.State != LedgerUncertain {
		t.Fatalf("state = %q, want uncertain (no evidence, unknown history)", outcome.State)
	}
	// Nothing was republished.
	newPath := filepath.Join(fsq.AgentInboxNew(base, "claude"), TransferFilename(env.SourceHost, env.TransferID))
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Fatalf("uncertain transfer was re-applied: %v", err)
	}
	// Uncertain is listed for status/doctor.
	uncertain, err := ledger.UnresolvedTransfers()
	if err != nil {
		t.Fatal(err)
	}
	if len(uncertain) != 1 || uncertain[0].TransferID != env.TransferID {
		t.Fatalf("uncertain list = %+v, want exactly this transfer", uncertain)
	}
}

func TestApplyWithLedgerPreparedWithCurEvidenceCommits(t *testing.T) {
	root, ledger := newLedgerTestRoot(t)
	base := rootRootDir(t, root)
	env := testEnvelope([]byte("hello"))

	// Crash history B: apply succeeded, consumer drained new->cur, the
	// committed append never happened. Plant the retained cur artifact.
	curPath := filepath.Join(fsq.AgentInboxCur(base, "claude"), TransferFilename(env.SourceHost, env.TransferID))
	if err := os.WriteFile(curPath, env.Payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ledger.appendRecord(ledgerRecord{
		Version:       ledgerSchemaVersion,
		State:         LedgerPrepared,
		SourceHost:    env.SourceHost,
		TransferID:    env.TransferID,
		PayloadSHA256: env.PayloadSHA256,
	}); err != nil {
		t.Fatal(err)
	}

	outcome, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if outcome.State != LedgerCommitted || !outcome.Replayed {
		t.Fatalf("state = %q replayed=%v, want committed via cur evidence", outcome.State, outcome.Replayed)
	}
}

func TestApplyWithLedgerPreparedWithDLQEvidenceCommits(t *testing.T) {
	root, ledger := newLedgerTestRoot(t)
	base := rootRootDir(t, root)
	env := testEnvelope([]byte("hello"))

	// Plant a DLQ envelope that WRAPS the original bytes under a new
	// id/filename (the DLQ never keeps the transfer filename).
	dlqEnv := fsq.DLQEnvelope{
		Schema:       fsq.DLQSchemaVersion,
		ID:           "dlqtest123456",
		OriginalID:   "orig",
		OriginalFile: TransferFilename(env.SourceHost, env.TransferID),
		SourceDir:    "inbox/new",
	}
	header, err := json.MarshalIndent(dlqEnv, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data := append([]byte("---\n"), header...)
	data = append(data, []byte("\n---\n")...)
	data = append(data, env.Payload...)
	dlqPath := filepath.Join(fsq.AgentDLQNew(base, "claude"), "dlqtest123456.md")
	if err := os.MkdirAll(fsq.AgentDLQNew(base, "claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dlqPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ledger.appendRecord(ledgerRecord{
		Version:       ledgerSchemaVersion,
		State:         LedgerPrepared,
		SourceHost:    env.SourceHost,
		TransferID:    env.TransferID,
		PayloadSHA256: env.PayloadSHA256,
	}); err != nil {
		t.Fatal(err)
	}

	outcome, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if outcome.State != LedgerCommitted {
		t.Fatalf("state = %q, want committed via DLQ original-content evidence", outcome.State)
	}
}

func TestApplyWithLedgerConflictDoesNotOverwriteWinner(t *testing.T) {
	root, ledger := newLedgerTestRoot(t)
	base := rootRootDir(t, root)
	env := testEnvelope([]byte("hello"))

	_, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	// Same transfer key, different digest: conflict observed in the outcome
	// only; the committed winner must remain the effective record.
	conflict := env
	other := []byte("different payload")
	sumBytes := sha256.Sum256(other)
	sum := hex.EncodeToString(sumBytes[:])
	conflict.Payload = other
	conflict.PayloadSHA256 = sum

	outcome, err := ApplyWithLedger(ledger, root, "mac", "claude", conflict)
	if err != nil {
		t.Fatalf("conflict apply: %v", err)
	}
	if outcome.Reason != "transfer_conflict" {
		t.Fatalf("conflict outcome reason = %q, want transfer_conflict", outcome.Reason)
	}
	// The original A replay is STILL committed with its own digest: the
	// conflicting B arrival never retired the winner.
	replay, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil {
		t.Fatalf("original replay after conflict: %v", err)
	}
	if replay.State != LedgerCommitted || !replay.Replayed || replay.Reason == "transfer_conflict" {
		t.Fatalf("original replay = %+v, want committed replayed without conflict", replay)
	}
	// Exactly one artifact remains in inbox/new with A's bytes.
	artifact := filepath.Join(fsq.AgentInboxNew(base, "claude"), TransferFilename(env.SourceHost, env.TransferID))
	data, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatalf("winner artifact missing: %v", err)
	}
	if string(data) != string(env.Payload) {
		t.Fatal("winner artifact bytes were replaced by the conflicting payload")
	}
}

func TestApplyWithLedgerPreparedBindingImmutableAgainstConflictingArrival(t *testing.T) {
	root, ledger := newLedgerTestRoot(t)
	base := rootRootDir(t, root)
	env := testEnvelope([]byte("hello"))

	// Crash history: prepared A, no evidence. Then B arrives under the same
	// key with a different digest.
	if err := ledger.appendRecord(ledgerRecord{
		Version:       ledgerSchemaVersion,
		State:         LedgerPrepared,
		SourceHost:    env.SourceHost,
		TransferID:    env.TransferID,
		PayloadSHA256: env.PayloadSHA256,
	}); err != nil {
		t.Fatal(err)
	}
	conflict := env
	other := []byte("different payload")
	sumBytes := sha256.Sum256(other)
	conflict.Payload = other
	conflict.PayloadSHA256 = hex.EncodeToString(sumBytes[:])

	outcome, err := ApplyWithLedger(ledger, root, "mac", "claude", conflict)
	if err != nil {
		t.Fatalf("conflicting arrival: %v", err)
	}
	if outcome.Reason != "transfer_conflict" {
		t.Fatalf("conflict outcome reason = %q, want transfer_conflict", outcome.Reason)
	}
	// Nothing from B was applied or recorded as the key's state.
	newPath := filepath.Join(fsq.AgentInboxNew(base, "claude"), TransferFilename(env.SourceHost, env.TransferID))
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Fatalf("conflicting payload applied: %v", err)
	}

	// A's own recovery is unblocked: A with evidence in DLQ/cur commits.
	curPath := filepath.Join(fsq.AgentInboxCur(base, "claude"), TransferFilename(env.SourceHost, env.TransferID))
	if err := os.WriteFile(curPath, env.Payload, 0o600); err != nil {
		t.Fatal(err)
	}
	recovered, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil {
		t.Fatalf("A recovery after conflict: %v", err)
	}
	if recovered.State != LedgerCommitted || !recovered.Replayed {
		t.Fatalf("A recovery = %+v, want committed replayed", recovered)
	}
}

func TestApplyWithLedgerSameNameDifferentBytesDoesNotPromote(t *testing.T) {
	// review-827-r1 P1a (mutation M3): the inbox evidence digest comparison
	// (:357) is load-bearing. A crash leaves a prepared record; a same-name
	// artifact holding DIFFERENT bytes sits in inbox/new (the reachable
	// pre-ledger case named at apply_file.go:96-98). The digest guard must
	// refuse promotion — the record stays prepared/uncertain, no receipt.
	root, ledger := newLedgerTestRoot(t)
	base := rootRootDir(t, root)
	env := testEnvelope([]byte("hello"))

	if err := ledger.appendRecord(ledgerRecord{
		Version:       ledgerSchemaVersion,
		State:         LedgerPrepared,
		SourceHost:    env.SourceHost,
		TransferID:    env.TransferID,
		PayloadSHA256: env.PayloadSHA256,
	}); err != nil {
		t.Fatal(err)
	}
	// Same filename, different bytes.
	newPath := filepath.Join(fsq.AgentInboxNew(base, "claude"), TransferFilename(env.SourceHost, env.TransferID))
	if err := os.WriteFile(newPath, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}

	outcome, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if outcome.State == LedgerCommitted {
		t.Fatalf("same-name/different-bytes artifact promoted prepared to committed — evidence digest guard missing")
	}
	// Nothing was re-applied either (no second artifact, no overwrite).
	data, err := os.ReadFile(newPath)
	if err != nil || string(data) != "tampered" {
		t.Fatalf("inbox artifact changed: %v", err)
	}
}

func TestApplyWithLedgerDLQOriginalContentDigestGuardsPromotion(t *testing.T) {
	// review-827-r1 P1a (mutation M4): the DLQ original-content digest
	// comparison (:321) is load-bearing. A DLQ envelope wrapping DIFFERENT
	// original bytes under the transfer filename must not promote a
	// prepared record to committed.
	root, ledger := newLedgerTestRoot(t)
	base := rootRootDir(t, root)
	env := testEnvelope([]byte("hello"))

	if err := ledger.appendRecord(ledgerRecord{
		Version:       ledgerSchemaVersion,
		State:         LedgerPrepared,
		SourceHost:    env.SourceHost,
		TransferID:    env.TransferID,
		PayloadSHA256: env.PayloadSHA256,
	}); err != nil {
		t.Fatal(err)
	}
	dlqEnv := fsq.DLQEnvelope{
		Schema:       fsq.DLQSchemaVersion,
		ID:           "dlq-poison01",
		OriginalID:   "orig",
		OriginalFile: TransferFilename(env.SourceHost, env.TransferID),
		SourceDir:    "inbox/new",
	}
	header, err := json.MarshalIndent(dlqEnv, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data := append([]byte("---\n"), header...)
	data = append(data, []byte("\n---\n")...)
	data = append(data, []byte("different original bytes")...) // NOT env.Payload
	dlqDir := fsq.AgentDLQNew(base, "claude")
	if err := os.MkdirAll(dlqDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dlqDir, "dlq-poison01.md"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	outcome, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if outcome.State == LedgerCommitted {
		t.Fatalf("DLQ envelope with different original content promoted prepared to committed — original-content digest guard missing")
	}
}

func TestApplyWithLedgerRetryableRejectedReappliesAndCommits(t *testing.T) {
	// review-827-r1 P0: an in-process apply failure is a PROVEN
	// non-delivery. It must be recorded as a retryable rejected outcome —
	// never left as a bare prepared that a later attempt reclassifies as
	// uncertain, permanently wedging the transfer. After the condition
	// clears, the retry must DELIVER (main's behaviour), not refuse.
	root, ledger := newLedgerTestRoot(t)
	base := rootRootDir(t, root)
	env := testEnvelope([]byte("hello"))

	// Make the apply fail: an unwritable inbox/tmp blocks the publication.
	tmpDir := filepath.Join(base, "agents", "claude", "inbox", "tmp")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmpDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tmpDir, 0o700) })

	outcome, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil {
		t.Fatalf("first apply (expected clean refusal, not error): %v", err)
	}
	if outcome.State != LedgerRejected {
		t.Fatalf("state = %q, want rejected with retryable reason on proven non-delivery", outcome.State)
	}
	if !strings.Contains(outcome.Reason, "apply failed (retryable)") {
		t.Fatalf("reason = %q, want a retryable apply-failure reason", outcome.Reason)
	}
	// The failure is durably recorded.
	records, torn, err := ledger.readRecords(env.SourceHost, env.TransferID)
	if err != nil || torn || len(records) < 2 || records[len(records)-1].State != LedgerRejected {
		t.Fatalf("records = %+v torn=%v err=%v, want prepared + retryable rejected", records, torn, err)
	}

	// Repair the condition and retry: the transfer must now deliver.
	if err := os.Chmod(tmpDir, 0o700); err != nil {
		t.Fatal(err)
	}
	recovered, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil {
		t.Fatalf("retry after repair: %v", err)
	}
	if recovered.State != LedgerCommitted {
		t.Fatalf("retry state = %q, want committed (proven non-delivery must stay recoverable)", recovered.State)
	}
	newPath := filepath.Join(fsq.AgentInboxNew(base, "claude"), TransferFilename(env.SourceHost, env.TransferID))
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("retry did not deliver the transfer: %v", err)
	}
}

func TestApplyWithLedgerErrExistFreshKeyIsTerminalRejected(t *testing.T) {
	// review-827-r1 P2-3: a fresh-key ErrExist must give ONE answer — a
	// durable terminal rejected record — not rejected-once then uncertain
	// forever from the leftover bare prepared.
	root, ledger := newLedgerTestRoot(t)
	base := rootRootDir(t, root)
	env := testEnvelope([]byte("hello"))

	// A same-name artifact with different bytes already occupies the slot.
	newPath := filepath.Join(fsq.AgentInboxNew(base, "claude"), TransferFilename(env.SourceHost, env.TransferID))
	if err := os.WriteFile(newPath, []byte("winner"), 0o600); err != nil {
		t.Fatal(err)
	}

	outcome, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if outcome.State != LedgerRejected {
		t.Fatalf("state = %q, want rejected", outcome.State)
	}
	// Second attempt: SAME terminal answer, not uncertain.
	again, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if again.State != LedgerRejected || again.Reason != outcome.Reason {
		t.Fatalf("second state = %q reason %q, want the same terminal rejection (got %q/%q first)", again.State, again.Reason, outcome.State, outcome.Reason)
	}
}

func TestApplyWithLedgerTornTailOverPreparedResolvesViaEvidence(t *testing.T) {
	// review-827-r1 P1c: a torn tail must not discard the intact prefix.
	// prepared record + torn append + digest-matching artifact in new =
	// resolvable; the refusal must not poison a recoverable transfer.
	root, ledger := newLedgerTestRoot(t)
	base := rootRootDir(t, root)
	env := testEnvelope([]byte("hello"))

	dir := filepath.Join(base, "bridge", "transfer-ledger", "mac_claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	line, err := ledgerLine(ledgerRecord{
		Version:       ledgerSchemaVersion,
		State:         LedgerPrepared,
		SourceHost:    env.SourceHost,
		TransferID:    env.TransferID,
		PayloadSHA256: env.PayloadSHA256,
	})
	if err != nil {
		t.Fatal(err)
	}
	fileData := append([]byte{}, line...)
	fileData = append(fileData, []byte("{\"version\":1,\"state\":\"comm")...) // torn tail
	if err := os.WriteFile(filepath.Join(dir, ledgerRecordName(env.SourceHost, env.TransferID)), fileData, 0o600); err != nil {
		t.Fatal(err)
	}
	// Digest-matching artifact retained in new.
	newPath := filepath.Join(fsq.AgentInboxNew(base, "claude"), TransferFilename(env.SourceHost, env.TransferID))
	if err := os.WriteFile(newPath, env.Payload, 0o600); err != nil {
		t.Fatal(err)
	}

	outcome, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil {
		t.Fatalf("apply on torn tail: %v", err)
	}
	if outcome.State != LedgerCommitted || !outcome.Replayed {
		t.Fatalf("state = %q replayed=%v, want committed replayed via evidence over torn tail", outcome.State, outcome.Replayed)
	}
	// Exactly one artifact (no duplicate).
	entries, err := os.ReadDir(fsq.AgentInboxNew(base, "claude"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("new entries = %d err=%v, want exactly 1", len(entries), err)
	}
}

func TestApplyWithLedgerTornTailAfterTerminalRecordGoverns(t *testing.T) {
	// review-827-r1 P1c, terminal case: a torn tail after a committed
	// record cannot un-terminal the commit — the intact prefix governs.
	root, ledger := newLedgerTestRoot(t)
	base := rootRootDir(t, root)
	env := testEnvelope([]byte("hello"))

	dir := filepath.Join(base, "bridge", "transfer-ledger", "mac_claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	prepLine, err := ledgerLine(ledgerRecord{
		Version: ledgerSchemaVersion, State: LedgerPrepared,
		SourceHost: env.SourceHost, TransferID: env.TransferID, PayloadSHA256: env.PayloadSHA256,
	})
	if err != nil {
		t.Fatal(err)
	}
	commitLine, err := ledgerLine(ledgerRecord{
		Version: ledgerSchemaVersion, State: LedgerCommitted,
		SourceHost: env.SourceHost, TransferID: env.TransferID, PayloadSHA256: env.PayloadSHA256,
	})
	if err != nil {
		t.Fatal(err)
	}
	fileData := append([]byte{}, prepLine...)
	fileData = append(fileData, commitLine...)
	fileData = append(fileData, []byte("{\"version\":1,\"sta")...) // torn tail
	if err := os.WriteFile(filepath.Join(dir, ledgerRecordName(env.SourceHost, env.TransferID)), fileData, 0o600); err != nil {
		t.Fatal(err)
	}

	outcome, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil {
		t.Fatalf("apply on committed + torn tail: %v", err)
	}
	if outcome.State != LedgerCommitted || !outcome.Replayed {
		t.Fatalf("state = %q replayed=%v, want committed replayed (terminal prefix governs torn tail)", outcome.State, outcome.Replayed)
	}
}

func TestApplyWithLedgerTornRecordIsUncertainNotAbsent(t *testing.T) {
	root, ledger := newLedgerTestRoot(t)
	base := rootRootDir(t, root)
	env := testEnvelope([]byte("hello"))

	// Write a torn (truncated) ledger record for the key.
	dir := filepath.Join(base, "bridge", "transfer-ledger", "mac_claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	torn := `{"version":1,"state":"prep` // truncated mid-JSON
	if err := os.WriteFile(filepath.Join(dir, ledgerRecordName(env.SourceHost, env.TransferID)), []byte(torn), 0o600); err != nil {
		t.Fatal(err)
	}

	outcome, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil {
		t.Fatalf("apply with torn record: %v", err)
	}
	if outcome.State != LedgerUncertain {
		t.Fatalf("state = %q, want uncertain for torn ledger state", outcome.State)
	}
	if !strings.Contains(outcome.Evidence, "torn") {
		t.Fatalf("evidence = %q, want torn-state note", outcome.Evidence)
	}
	// Nothing was applied.
	newPath := filepath.Join(fsq.AgentInboxNew(base, "claude"), TransferFilename(env.SourceHost, env.TransferID))
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Fatalf("torn-state transfer was applied: %v", err)
	}
}

func TestApplyWithLedgerPreLedgerCurArtifactIsBoundNotReapplied(t *testing.T) {
	root, ledger := newLedgerTestRoot(t)
	base := rootRootDir(t, root)
	env := testEnvelope([]byte("hello"))

	// Pre-ledger crash history: A was applied and drained new->cur BEFORE any
	// ledger existed, so there is no record for the key. A replay must NOT
	// re-apply (which would duplicate into new), and the replay itself is the
	// evidence inspection that binds and commits the key.
	curPath := filepath.Join(fsq.AgentInboxCur(base, "claude"), TransferFilename(env.SourceHost, env.TransferID))
	if err := os.WriteFile(curPath, env.Payload, 0o600); err != nil {
		t.Fatal(err)
	}

	outcome, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil {
		t.Fatalf("replay of pre-ledger drained transfer: %v", err)
	}
	if outcome.State != LedgerCommitted || !outcome.Replayed {
		t.Fatalf("state = %q replayed=%v, want committed replayed via retained cur evidence", outcome.State, outcome.Replayed)
	}
	// No new copy was published into inbox/new.
	newPath := filepath.Join(fsq.AgentInboxNew(base, "claude"), TransferFilename(env.SourceHost, env.TransferID))
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Fatalf("pre-ledger replay re-published into new: %v", err)
	}
	// The binding is now durable and correct (A's digest, not something else).
	rec, torn, err := ledger.effectiveRecord(env.SourceHost, env.TransferID)
	if err != nil || torn || rec == nil || rec.State != LedgerCommitted || rec.PayloadSHA256 != env.PayloadSHA256 {
		t.Fatalf("post-replay record = %+v torn=%v err=%v, want committed with A's digest", rec, torn, err)
	}
}

func TestApplyWithLedgerPreLedgerDrainedReplayDoesNotBindConflictingArrivalAway(t *testing.T) {
	root, ledger := newLedgerTestRoot(t)
	base := rootRootDir(t, root)
	env := testEnvelope([]byte("hello"))

	// Pre-ledger drained artifact (cur) for A; then B (different digest)
	// arrives FIRST with no ledger history. B must not bind the key away
	// from the legitimate winner's retained evidence: B is refused, A's
	// binding/evidence stays recoverable.
	curPath := filepath.Join(fsq.AgentInboxCur(base, "claude"), TransferFilename(env.SourceHost, env.TransferID))
	if err := os.WriteFile(curPath, env.Payload, 0o600); err != nil {
		t.Fatal(err)
	}

	other := []byte("different payload")
	sumBytes := sha256.Sum256(other)
	conflict := env
	conflict.Payload = other
	conflict.PayloadSHA256 = hex.EncodeToString(sumBytes[:])

	outcome, err := ApplyWithLedger(ledger, root, "mac", "claude", conflict)
	if err != nil {
		t.Fatalf("conflicting first arrival over retained evidence: %v", err)
	}
	if outcome.State != LedgerRejected || outcome.Reason != "transfer_conflict" {
		t.Fatalf("conflict outcome = %+v, want rejected transfer_conflict (loser reported unambiguously)", outcome)
	}
	// B was not applied into new.
	newPath := filepath.Join(fsq.AgentInboxNew(base, "claude"), TransferFilename(env.SourceHost, env.TransferID))
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Fatalf("conflicting payload applied: %v", err)
	}
	// No ledger record was created for the key (the winner's evidence was
	// never poisoned by a B binding).
	rec, torn, err := ledger.effectiveRecord(env.SourceHost, env.TransferID)
	if err != nil || torn {
		t.Fatalf("winner-key ledger state corrupted: rec=%+v torn=%v err=%v", rec, torn, err)
	}

	// A's own replay still recovers to committed via the retained cur copy.
	recovered, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil {
		t.Fatalf("A recovery after B conflict: %v", err)
	}
	if recovered.State != LedgerCommitted || !recovered.Replayed {
		t.Fatalf("A recovery = %+v, want committed replayed", recovered)
	}
}

func TestApplyWithLedgerPreLedgerDLQArtifactIsBoundNotReapplied(t *testing.T) {
	root, ledger := newLedgerTestRoot(t)
	base := rootRootDir(t, root)
	env := testEnvelope([]byte("hello"))

	// Pre-ledger DLQ history: A was DLQ'd (wrapped under a new id/filename)
	// before any ledger existed. A replay must bind+commit from the DLQ
	// original-content evidence, never re-apply.
	dlqEnv := fsq.DLQEnvelope{
		Schema:       fsq.DLQSchemaVersion,
		ID:           "dlqlegacy01",
		OriginalID:   "orig",
		OriginalFile: TransferFilename(env.SourceHost, env.TransferID),
		SourceDir:    "inbox/new",
	}
	header, err := json.MarshalIndent(dlqEnv, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data := append([]byte("---\n"), header...)
	data = append(data, []byte("\n---\n")...)
	data = append(data, env.Payload...)
	dlqDir := fsq.AgentDLQNew(base, "claude")
	if err := os.MkdirAll(dlqDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dlqDir, "dlqlegacy01.md"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	outcome, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil {
		t.Fatalf("replay of pre-ledger DLQ'd transfer: %v", err)
	}
	if outcome.State != LedgerCommitted || !outcome.Replayed {
		t.Fatalf("state = %q replayed=%v, want committed replayed via DLQ evidence", outcome.State, outcome.Replayed)
	}
	newPath := filepath.Join(fsq.AgentInboxNew(base, "claude"), TransferFilename(env.SourceHost, env.TransferID))
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Fatalf("pre-ledger DLQ replay re-published into new: %v", err)
	}
}

func TestAppendLedgerLineDirectoryDurabilityForEmptyOrphan(t *testing.T) {
	root, ledger := newLedgerTestRoot(t)
	base := rootRootDir(t, root)
	env := testEnvelope([]byte("hello"))

	ledgerDir := filepath.Join(base, "bridge", "transfer-ledger", "mac_claude")
	if err := os.MkdirAll(ledgerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	name := ledgerRecordName(env.SourceHost, env.TransferID)

	// Observed crash sequence: an earlier O_EXCL create produced an EMPTY
	// ledger file and the process died before write+sync (readRecords treats
	// the empty file as absent, so a fresh prepared would take the existing-
	// file append path and, without this fix, skip directory sync).
	if err := os.WriteFile(filepath.Join(ledgerDir, name), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	// Track that directory-chain durability actually runs for the ledger
	// file on this append (fsq's platform sync is stubbed here via the
	// exported test hook, so observe through it deterministically).
	var mu sync.Mutex
	var synced []string
	root.SetSyncDirFaultForTest(func(dir string) error {
		mu.Lock()
		synced = append(synced, dir)
		mu.Unlock()
		return nil
	})
	t.Cleanup(func() { root.SetSyncDirFaultForTest(nil) })

	if err := ledger.appendRecordFirstWrite(ledgerRecord{
		Version:       ledgerSchemaVersion,
		State:         LedgerPrepared,
		SourceHost:    env.SourceHost,
		TransferID:    env.TransferID,
		PayloadSHA256: env.PayloadSHA256,
	}); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	wantLedgerDir := filepath.Join("bridge", "transfer-ledger", "mac_claude")
	var sawChain, sawDot bool
	for _, dir := range synced {
		if dir == wantLedgerDir {
			sawChain = true
		}
		if dir == "." {
			sawDot = true
		}
	}
	if !sawChain || !sawDot {
		t.Fatalf("directory durability not established on empty-orphan append: synced=%v (sawChain=%v sawDot=%v)", synced, sawChain, sawDot)
	}
	// The record is readable through the ledger (the append landed).
	records, torn, err := ledger.readRecords(env.SourceHost, env.TransferID)
	if err != nil || torn || len(records) != 1 || records[0].State != LedgerPrepared {
		t.Fatalf("records = %+v torn=%v err=%v, want one prepared record", records, torn, err)
	}
}

func TestAppendLedgerLineRepairsFailedDirectorySyncOnRetry(t *testing.T) {
	root, ledger := newLedgerTestRoot(t)
	base := rootRootDir(t, root)
	env := testEnvelope([]byte("hello"))

	ledgerDir := filepath.Join(base, "bridge", "transfer-ledger", "mac_claude")
	if err := os.MkdirAll(ledgerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Observed failure mode (verifier r4 P1): a first append writes+fsyncs a
	// complete NONEMPTY prepared record, then the directory sync fails. The
	// file's name is on disk but NOT durable. A later append must repair the
	// guarantee — regardless of the file's size or the created flag — before
	// recovery can trust the binding.
	var mu sync.Mutex
	failLedgerDirSync := true
	var synced []string
	root.SetSyncDirFaultForTest(func(dir string) error {
		mu.Lock()
		defer mu.Unlock()
		wantLedgerDir := filepath.Join("bridge", "transfer-ledger", "mac_claude")
		if failLedgerDirSync && dir == wantLedgerDir {
			return errors.New("injected: directory sync failed")
		}
		synced = append(synced, dir)
		return nil
	})
	t.Cleanup(func() { root.SetSyncDirFaultForTest(nil) })

	firstErr := ledger.appendRecordFirstWrite(ledgerRecord{
		Version:       ledgerSchemaVersion,
		State:         LedgerPrepared,
		SourceHost:    env.SourceHost,
		TransferID:    env.TransferID,
		PayloadSHA256: env.PayloadSHA256,
	})
	if firstErr == nil {
		t.Fatalf("append with injected dir-sync failure: want error, got nil")
	}

	// Retry: the failure injection is lifted. The append must (re)establish
	// the full chain — the ledger dir, every ancestor, AND root-relative
	// dot — even though the file already exists and is nonempty.
	mu.Lock()
	failLedgerDirSync = false
	synced = nil
	mu.Unlock()
	if err := ledger.appendRecordFirstWrite(ledgerRecord{
		Version:       ledgerSchemaVersion,
		State:         LedgerUncertain,
		SourceHost:    env.SourceHost,
		TransferID:    env.TransferID,
		PayloadSHA256: env.PayloadSHA256,
		Reason:        "retried after failed dir sync",
	}); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	wantLedgerDir := filepath.Join("bridge", "transfer-ledger", "mac_claude")
	var sawSelf, sawBridge, sawBridgeParent, sawDot bool
	for _, dir := range synced {
		switch dir {
		case wantLedgerDir:
			sawSelf = true
		case filepath.Join("bridge", "transfer-ledger"):
			sawBridgeParent = true
		case "bridge":
			sawBridge = true
		case ".":
			sawDot = true
		}
	}
	if !sawSelf || !sawBridgeParent || !sawBridge || !sawDot {
		t.Fatalf("retry after failed dir sync did not repair the full directory chain: synced=%v (self=%v ledgerParent=%v bridge=%v dot=%v)", synced, sawSelf, sawBridgeParent, sawBridge, sawDot)
	}
	// Both records survived: the prepared intent and the retry.
	records, torn, err := ledger.readRecords(env.SourceHost, env.TransferID)
	if err != nil || torn || len(records) != 2 || records[0].State != LedgerPrepared || records[1].State != LedgerUncertain {
		t.Fatalf("records = %+v torn=%v err=%v, want prepared then uncertain", records, torn, err)
	}
}

func TestApplyWithLedgerSerializesConcurrentArrivals(t *testing.T) {
	root, ledger := newLedgerTestRoot(t)
	env := testEnvelope([]byte("hello"))

	const workers = 8
	errs := make(chan error, workers)
	uncertain := make(chan int, workers)
	for i := 0; i < workers; i++ {
		go func() {
			outcome, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
			if err != nil {
				errs <- err
				return
			}
			if outcome.State == LedgerUncertain {
				uncertain <- 1
			} else {
				uncertain <- 0
			}
			errs <- nil
		}()
	}
	uncertainCount := 0
	for i := 0; i < workers; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent apply: %v", err)
		}
		if <-uncertain == 1 {
			uncertainCount++
		}
	}
	// Every caller observes a terminal state: exactly one fresh commit and
	// the rest replays — or, if a loser observed prepared-without-evidence
	// mid-race, it must be uncertain, never a duplicate publish. Count
	// inbox/new artifacts.
	entries, err := os.ReadDir(fsq.AgentInboxNew(rootRootDir(t, root), "claude"))
	if err != nil {
		t.Fatal(err)
	}
	matches := 0
	for _, entry := range entries {
		if entry.Name() == TransferFilename(env.SourceHost, env.TransferID) {
			matches++
		}
	}
	if matches != 1 {
		t.Fatalf("inbox/new has %d copies of the transfer artifact, want exactly 1 (uncertain=%d)", matches, uncertainCount)
	}
}

func TestApplyWithLedgerRejectsBadSession(t *testing.T) {
	base := t.TempDir()
	identity, err := fsq.SnapshotDeliveryRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	root, err := fsq.OpenDeliveryRoot(base, identity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	if _, err := NewTransferLedger(root, "mac/claude/extra"); err == nil {
		t.Fatal("expected session validation failure")
	}
	if _, err := NewTransferLedger(root, ""); err == nil {
		t.Fatal("expected empty session rejection")
	}
}
