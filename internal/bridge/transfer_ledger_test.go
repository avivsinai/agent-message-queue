package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	return root.Base()
}

func newLedgerTestRoot(t *testing.T) (*fsq.DeliveryRoot, *TransferLedger) {
	t.Helper()
	base := t.TempDir()
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

	if err := ledger.appendRecord(ledgerRecord{
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

	firstErr := ledger.appendRecord(ledgerRecord{
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
	if err := ledger.appendRecord(ledgerRecord{
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

// TestApplyWithLedgerCommittedDurabilityErrorIsCommittedNotRetryable
// (review-827-r2 P0): DeliverToExistingInbox returning
// *fsq.CommittedDurabilityError means the rename into inbox/new SUCCEEDED —
// the message IS delivered. The ledger must record committed (with the
// error's FinalPath), and a later same-digest apply must be an idempotent
// replay — never a retryable rejection whose re-apply duplicates the
// delivery into new AND cur.
func TestApplyWithLedgerCommittedDurabilityErrorIsCommittedNotRetryable(t *testing.T) {
	root, ledger := newLedgerTestRoot(t)
	base := rootRootDir(t, root)
	env := testEnvelope([]byte("cde-payload"))

	root.SetSyncDirFaultForTest(func(dir string) error {
		if strings.HasSuffix(dir, filepath.Join("inbox", "new")) {
			return fmt.Errorf("injected EIO")
		}
		return nil
	})

	outcome, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	// Codex r2-r2 finding 1: published but durability UNKNOWN — a distinct
	// state, NOT durable success. No receipt/ACK flows for it (courier maps
	// it to a refusal), and the ledger never forgets the publication fact.
	if outcome.State != LedgerPublishedDurabilityUnknown {
		t.Fatalf("state = %q (%s), want published_durability_unknown", outcome.State, outcome.Reason)
	}
	if outcome.Path == "" {
		t.Fatalf("published outcome lost FinalPath")
	}
	// The artifact IS visible in new (publication happened).
	entries, err := os.ReadDir(fsq.AgentInboxNew(base, "claude"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("new entries = %d err=%v, want 1 (published)", len(entries), err)
	}
	// While the sync fault persists, a retry must NOT re-apply (no duplicate)
	// and must stay in the withheld state.
	outcomeAgain, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil {
		t.Fatalf("re-apply under fault: %v", err)
	}
	if outcomeAgain.State != LedgerPublishedDurabilityUnknown || !outcomeAgain.Replayed {
		t.Fatalf("re-apply under fault: state=%q replayed=%v, want published_durability_unknown replayed (no re-apply)", outcomeAgain.State, outcomeAgain.Replayed)
	}
	newEntries, _ := os.ReadDir(fsq.AgentInboxNew(base, "claude"))
	if len(newEntries) != 1 {
		t.Fatalf("DUPLICATE under fault: new=%d, want 1", len(newEntries))
	}
	// Repair the destination sync, then re-verify: promotes to committed
	// WITHOUT re-applying (drain the artifact first to prove no re-apply).
	root.SetSyncDirFaultForTest(nil)
	src := filepath.Join(fsq.AgentInboxNew(base, "claude"), newEntries[0].Name())
	dst := filepath.Join(fsq.AgentInboxCur(base, "claude"), newEntries[0].Name())
	if err := os.Rename(src, dst); err != nil {
		t.Fatal(err)
	}
	outcome2, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil {
		t.Fatalf("post-repair: %v", err)
	}
	if outcome2.State != LedgerCommitted || !outcome2.Replayed {
		t.Fatalf("post-repair state = %q replayed=%v, want committed replay (durability verified, no re-apply)", outcome2.State, outcome2.Replayed)
	}
	// No duplicate: still one artifact (now in cur).
	newEntries2, _ := os.ReadDir(fsq.AgentInboxNew(base, "claude"))
	curEntries, _ := os.ReadDir(fsq.AgentInboxCur(base, "claude"))
	if len(newEntries2)+len(curEntries) != 1 {
		t.Fatalf("DUPLICATE DELIVERY: new=%d cur=%d, want 1 total", len(newEntries2), len(curEntries))
	}
}

// TestApplyWithLedgerRetryableRejectedResolvesViaEvidence (review-827-r2 P0,
// retry arm): a rejected(retryable) record whose delivery evidence appeared
// in cur (consumer drain / operator repair) must promote via evidence, not
// re-apply and duplicate.
func TestApplyWithLedgerRetryableRejectedResolvesViaEvidence(t *testing.T) {
	root, ledger := newLedgerTestRoot(t)
	base := rootRootDir(t, root)
	env := testEnvelope([]byte("retry-evidence"))

	if err := ledger.appendRecord(ledgerRecord{
		Version:       ledgerSchemaVersion,
		State:         LedgerRejected,
		SourceHost:    env.SourceHost,
		TransferID:    env.TransferID,
		PayloadSHA256: env.PayloadSHA256,
		Retryable:     true,
		Reason:        "apply failed (retryable): injected",
	}); err != nil {
		t.Fatal(err)
	}
	// Evidence appears in cur.
	curPath := filepath.Join(fsq.AgentInboxCur(base, "claude"), TransferFilename(env.SourceHost, env.TransferID))
	if err := os.WriteFile(curPath, env.Payload, 0o600); err != nil {
		t.Fatal(err)
	}

	outcome, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if outcome.State != LedgerCommitted || !outcome.Replayed {
		t.Fatalf("state = %q replayed=%v, want evidence-promoted committed replay", outcome.State, outcome.Replayed)
	}
	// No re-apply: still exactly the one retained artifact.
	newEntries, _ := os.ReadDir(fsq.AgentInboxNew(base, "claude"))
	curEntries, _ := os.ReadDir(fsq.AgentInboxCur(base, "claude"))
	if len(newEntries)+len(curEntries) != 1 {
		t.Fatalf("DUPLICATE: new=%d cur=%d, want 1 total", len(newEntries), len(curEntries))
	}
}

// TestApplyWithLedgerRetryReArmsPreparedIntent (codex batch finding 2,
// review-827-r2): an authorized retry must append a FRESH durable prepared
// intent before applying, so a crash between retry-success and the commit
// append resolves via the prepared binding + publication evidence instead of
// reading terminal-rejected while the delivery may be live.
func TestApplyWithLedgerRetryReArmsPreparedIntent(t *testing.T) {
	root, ledger := newLedgerTestRoot(t)
	base := rootRootDir(t, root)
	env := testEnvelope([]byte("rearm-payload"))

	if err := ledger.appendRecord(ledgerRecord{
		Version:       ledgerSchemaVersion,
		State:         LedgerRejected,
		SourceHost:    env.SourceHost,
		TransferID:    env.TransferID,
		PayloadSHA256: env.PayloadSHA256,
		Retryable:     true,
		Reason:        "apply failed (retryable): injected",
	}); err != nil {
		t.Fatal(err)
	}

	outcome, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if outcome.State != LedgerCommitted {
		t.Fatalf("state = %q (%s), want committed", outcome.State, outcome.Reason)
	}
	// The ledger history must now contain prepared ... rejected ... prepared
	// ... committed: the retry re-armed the intent. Read the raw file.
	data, err := os.ReadFile(filepath.Join(base, "bridge", "transfer-ledger", "mac_claude", ledgerRecordName(env.SourceHost, env.TransferID)))
	if err != nil {
		t.Fatal(err)
	}
	states := ledgerStatesIn(string(data))
	if len(states) != 3 || states[0] != "rejected" || states[1] != "prepared" || states[2] != "committed" {
		t.Fatalf("ledger states = %v, want [rejected prepared committed] (retry re-armed a prepared intent before applying)", states)
	}
}

// ledgerStatesIn extracts the state field of every valid JSON line.
func ledgerStatesIn(s string) []string {
	var states []string
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		var rec struct {
			State string `json:"state"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.State != "" {
			states = append(states, rec.State)
		}
	}
	return states
}

// TestApplyWithLedgerTornTailPromotionIsSeparatelyReadable (codex batch
// finding 3, review-827-r2): a committed promotion over a torn tail must be
// framed so it is READABLE on the next reread — appending JSON directly into
// the unterminated fragment concatenates lines and the reader discards the
// promotion with the artifact.
func TestApplyWithLedgerTornTailPromotionIsSeparatelyReadable(t *testing.T) {
	root, ledger := newLedgerTestRoot(t)
	base := rootRootDir(t, root)
	env := testEnvelope([]byte("torn-promote"))

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
	ledgerPath := filepath.Join(dir, ledgerRecordName(env.SourceHost, env.TransferID))
	if err := os.WriteFile(ledgerPath, fileData, 0o600); err != nil {
		t.Fatal(err)
	}
	// Digest-matching artifact retained in new.
	newPath := filepath.Join(fsq.AgentInboxNew(base, "claude"), TransferFilename(env.SourceHost, env.TransferID))
	if err := os.WriteFile(newPath, env.Payload, 0o600); err != nil {
		t.Fatal(err)
	}

	outcome, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if outcome.State != LedgerCommitted || !outcome.Replayed {
		t.Fatalf("state = %q replayed=%v, want committed replay via evidence", outcome.State, outcome.Replayed)
	}
	// THE PIN: re-apply. The promotion must still be readable — the replay
	// resolves committed from the ledger record, not by re-deriving from
	// the artifact, and the artifact count stays 1 even if the artifact is
	// removed after this point.
	data, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	states := ledgerStatesIn(string(data))
	if len(states) < 2 || states[len(states)-1] != "committed" {
		t.Fatalf("reread ledger states = %v, want a readable terminal committed record after the torn tail", states)
	}
	if err := os.Remove(newPath); err != nil {
		t.Fatal(err)
	}
	outcome2, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil {
		t.Fatalf("replay after artifact removal: %v", err)
	}
	if outcome2.State != LedgerCommitted || !outcome2.Replayed {
		t.Fatalf("post-removal state = %q replayed=%v, want committed replay from the framed ledger record", outcome2.State, outcome2.Replayed)
	}
}

// TestApplyWithLedgerUnprovenCollisionIsUncertainNotConflict (codex r2-r2
// finding 2): an os.ErrExist whose existing bytes cannot be READ is neither a
// proven non-delivery nor a proven conflict — the ledger records uncertain,
// never terminal transfer_conflict and never a retryable re-apply.
func TestApplyWithLedgerUnprovenCollisionIsUncertainNotConflict(t *testing.T) {
	root, ledger := newLedgerTestRoot(t)
	base := rootRootDir(t, root)
	env := testEnvelope([]byte("collision-unproven"))

	// Seed a retryable rejection so the retry branch runs (it skips the
	// occupancy pre-check and reaches the apply), pre-place a same-name
	// artifact with DIFFERENT readable bytes (so the evidence scan passes
	// without finding this digest — it is not evidence), then force the
	// apply-time collision READ to fail: the bytes are unprovable at the
	// collision site. This is the exact classifier path codex r2-r3 finding 2
	// required: ErrCollisionUnproven checked independently of os.ErrExist.
	if err := ledger.appendRecord(ledgerRecord{
		Version:       ledgerSchemaVersion,
		State:         LedgerRejected,
		SourceHost:    env.SourceHost,
		TransferID:    env.TransferID,
		PayloadSHA256: env.PayloadSHA256,
		Retryable:     true,
		Reason:        "apply failed (retryable): injected",
	}); err != nil {
		t.Fatal(err)
	}
	slot := filepath.Join(fsq.AgentInboxNew(base, "claude"), TransferFilename(env.SourceHost, env.TransferID))
	if err := os.WriteFile(slot, []byte("different bytes entirely"), 0o600); err != nil {
		t.Fatal(err)
	}
	targetReads := 0
	var hook func(string) ([]byte, error)
	hook = func(name string) ([]byte, error) {
		if strings.HasSuffix(name, TransferFilename(env.SourceHost, env.TransferID)) {
			// First read is the pre-apply evidence scan (must see the
			// different bytes and pass on); the SECOND is the apply-time
			// collision read — force it to fail there.
			targetReads++
			if targetReads >= 2 {
				return nil, fmt.Errorf("forced unreadable collision")
			}
		}
		// Bypass the hook for non-target reads (single-threaded test: safe).
		root.SetReadRegularNoFollowFaultForTest(nil)
		defer root.SetReadRegularNoFollowFaultForTest(hook)
		return root.ReadRegularNoFollow(name)
	}
	root.SetReadRegularNoFollowFaultForTest(hook)
	t.Cleanup(func() { root.SetReadRegularNoFollowFaultForTest(nil) })

	outcome, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if outcome.State != LedgerUncertain {
		t.Fatalf("state = %q (%s), want uncertain (unprovable collision)", outcome.State, outcome.Evidence)
	}
	// The durable record proves the CLASSIFIER fired (uncertain with the
	// collision reason), not the occupied-slot refusal (that would be
	// terminal rejected) and not the evidence path (that records nothing).
	recs, _, err := ledger.readRecords(env.SourceHost, env.TransferID)
	if err != nil || len(recs) == 0 {
		t.Fatalf("records = %v err=%v, want the classifier's uncertain record", recs, err)
	}
	last := recs[len(recs)-1]
	if last.State != LedgerUncertain || last.Retryable || !strings.Contains(last.Reason, "collision bytes unreadable") {
		t.Fatalf("last record = %+v, want uncertain non-retryable from the collision classifier", last)
	}
	// Nothing was applied: the unreadable slot artifact blocks delivery.
	newEntries, _ := os.ReadDir(fsq.AgentInboxNew(base, "claude"))
	if len(newEntries) != 1 {
		t.Fatalf("new entries = %d, want just the pre-existing slot artifact", len(newEntries))
	}
	if outcome2, err := ApplyWithLedger(ledger, root, "mac", "claude", env); err != nil || outcome2.State != LedgerUncertain {
		t.Fatalf("retry = %q err=%v, want stable uncertain", outcome2.State, err)
	}
}

// TestApplyWithLedgerTornTailOverRetryableRejectedNeverApplies (codex r2-r2
// finding 3): a rejected(Retryable=true) prefix followed by a torn tail is
// NOT terminal history. Without publication evidence the transfer must stay
// uncertain — never re-apply from ambiguous torn history.
func TestApplyWithLedgerTornTailOverRetryableRejectedNeverApplies(t *testing.T) {
	root, ledger := newLedgerTestRoot(t)
	base := rootRootDir(t, root)
	env := testEnvelope([]byte("torn-retryable"))

	if err := ledger.appendRecord(ledgerRecord{
		Version:       ledgerSchemaVersion,
		State:         LedgerRejected,
		SourceHost:    env.SourceHost,
		TransferID:    env.TransferID,
		PayloadSHA256: env.PayloadSHA256,
		Retryable:     true,
		Reason:        "apply failed (retryable): injected",
	}); err != nil {
		t.Fatal(err)
	}
	// Tear the tail: partial garbage line appended after the valid record.
	recPath := filepath.Join(base, "bridge", "transfer-ledger", "mac_claude", ledgerRecordName(env.SourceHost, env.TransferID))
	f, err := os.OpenFile(recPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"version":2,"state":"comm`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	outcome, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if outcome.State != LedgerUncertain {
		t.Fatalf("state = %q (%s), want uncertain (torn tail over retryable rejection, no evidence)", outcome.State, outcome.Evidence)
	}
	// Nothing was applied: no artifact in new or cur.
	newEntries, _ := os.ReadDir(fsq.AgentInboxNew(base, "claude"))
	curEntries, _ := os.ReadDir(fsq.AgentInboxCur(base, "claude"))
	if len(newEntries)+len(curEntries) != 0 {
		t.Fatalf("APPLIED FROM TORN HISTORY: new=%d cur=%d, want 0", len(newEntries), len(curEntries))
	}
	// WITH evidence, the torn history still resolves cleanly via the framed
	// promotion: durable committed, no re-apply.
	curPath := filepath.Join(fsq.AgentInboxCur(base, "claude"), TransferFilename(env.SourceHost, env.TransferID))
	if err := os.WriteFile(curPath, env.Payload, 0o600); err != nil {
		t.Fatal(err)
	}
	outcome2, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil {
		t.Fatalf("evidence apply: %v", err)
	}
	if outcome2.State != LedgerCommitted || !outcome2.Replayed {
		t.Fatalf("state = %q replayed=%v, want evidence-promoted committed replay from torn history", outcome2.State, outcome2.Replayed)
	}
	// The promotion is separately readable after the torn line.
	recs, _, err := ledger.readRecords(env.SourceHost, env.TransferID)
	if err != nil {
		t.Fatalf("reread: %v", err)
	}
	if len(recs) == 0 || recs[len(recs)-1].State != LedgerCommitted {
		t.Fatalf("last record = %+v, want committed readable after torn prefix", recs)
	}
}

// TestApplyWithLedgerTornRecoveryPromotionRespectsCarrierSyncFault (codex
// r2-r3 follow-up, the concrete blocker): torn nonterminal recovery over a
// prepared prefix must go through the shared evidence-promotion path — if
// the evidence carrier's sync still faults, the recovery must yield
// published_durability_unknown (receipt/ACK withheld), never a torn-framed
// committed record over an unsynced carrier.
func TestApplyWithLedgerTornRecoveryPromotionRespectsCarrierSyncFault(t *testing.T) {
	root, ledger := newLedgerTestRoot(t)
	base := rootRootDir(t, root)
	env := testEnvelope([]byte("torn-carrier-fault"))

	if err := ledger.appendRecord(ledgerRecord{
		Version:       ledgerSchemaVersion,
		State:         LedgerPrepared,
		SourceHost:    env.SourceHost,
		TransferID:    env.TransferID,
		PayloadSHA256: env.PayloadSHA256,
	}); err != nil {
		t.Fatal(err)
	}
	// Tear the tail over the prepared prefix.
	recPath := filepath.Join(base, "bridge", "transfer-ledger", "mac_claude", ledgerRecordName(env.SourceHost, env.TransferID))
	f, err := os.OpenFile(recPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"version":2,"state":"comm`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	// Consumer drained the artifact to cur (digest-verified evidence).
	curPath := filepath.Join(fsq.AgentInboxCur(base, "claude"), TransferFilename(env.SourceHost, env.TransferID))
	if err := os.WriteFile(curPath, env.Payload, 0o600); err != nil {
		t.Fatal(err)
	}
	// The cur carrier's sync still faults (the ongoing durability problem).
	faults := 0
	root.SetSyncDirFaultForTest(func(dir string) error {
		if strings.HasSuffix(dir, filepath.Join("inbox", "cur")) {
			faults++
			return fmt.Errorf("injected EIO on cur")
		}
		return nil
	})
	t.Cleanup(func() { root.SetSyncDirFaultForTest(nil) })

	outcome, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if outcome.State != LedgerPublishedDurabilityUnknown || faults == 0 {
		t.Fatalf("state = %q faults=%d, want published_durability_unknown via a faulted carrier sync (no torn committed over unsynced carrier)", outcome.State, faults)
	}
	// No committed record readable (torn or otherwise).
	recs, _, err := ledger.readRecords(env.SourceHost, env.TransferID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.State == LedgerCommitted {
			t.Fatalf("committed record recorded over a faulted carrier: %+v", r)
		}
	}
	// Repair the sync: promotion succeeds, framed and separately readable.
	root.SetSyncDirFaultForTest(nil)
	outcome2, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil || outcome2.State != LedgerCommitted || !outcome2.Replayed {
		t.Fatalf("post-repair = %q err=%v replayed=%v, want framed committed promotion", outcome2.State, err, outcome2.Replayed)
	}
	recs2, _, err := ledger.readRecords(env.SourceHost, env.TransferID)
	if err != nil || len(recs2) == 0 || recs2[len(recs2)-1].State != LedgerCommitted {
		t.Fatalf("post-repair records = %v err=%v, want last record committed", recs2, err)
	}
}

// TestApplyWithLedgerCarrierSyncFailureRevokesRetryPermission: publication
// observed but the carrier directory cannot be verified durable. The ledger
// must durably record published_durability_unknown BEFORE the outcome
// returns — a returned struct is not persistent state — so a later call
// after the retained artifact is consumed re-enters at published-unknown
// (which never re-applies) instead of reading the stale retryable-rejected
// record and duplicating the delivery.
func TestApplyWithLedgerCarrierSyncFailureRevokesRetryPermission(t *testing.T) {
	root, ledger := newLedgerTestRoot(t)
	base := rootRootDir(t, root)
	env := testEnvelope([]byte("revoke-retry-payload"))

	// Start from a retryable-rejected state whose delivery later shows up in
	// cur (consumer drain / operator repair of the earlier failure).
	if err := ledger.appendRecord(ledgerRecord{
		Version:       ledgerSchemaVersion,
		State:         LedgerRejected,
		SourceHost:    env.SourceHost,
		TransferID:    env.TransferID,
		PayloadSHA256: env.PayloadSHA256,
		Retryable:     true,
		Reason:        "apply failed (retryable): injected",
	}); err != nil {
		t.Fatal(err)
	}
	curPath := filepath.Join(fsq.AgentInboxCur(base, "claude"), TransferFilename(env.SourceHost, env.TransferID))
	if err := os.WriteFile(curPath, env.Payload, 0o600); err != nil {
		t.Fatal(err)
	}

	// Carrier sync fails only on the evidence carrier (inbox/cur): the
	// ledger appends themselves must succeed so the durable revocation can
	// land — the sequence under test is a failing CARRIER, not a failing
	// ledger file.
	carrierRel := filepath.Join("agents", "claude", "inbox", "cur")
	root.SetSyncDirFaultForTest(func(dir string) error {
		if dir == carrierRel || strings.HasPrefix(filepath.ToSlash(filepath.Clean(dir)), filepath.ToSlash(carrierRel)+"/") {
			return errors.New("injected: carrier sync failed")
		}
		return nil
	})
	t.Cleanup(func() { root.SetSyncDirFaultForTest(nil) })

	outcome, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if outcome.State != LedgerPublishedDurabilityUnknown {
		t.Fatalf("state = %q, want published_durability_unknown", outcome.State)
	}
	root.SetSyncDirFaultForTest(nil)

	// The durable ledger (not the returned struct) must now carry the
	// published-unknown transition: read the raw file.
	data, err := os.ReadFile(filepath.Join(base, "bridge", "transfer-ledger", "mac_claude", ledgerRecordName(env.SourceHost, env.TransferID)))
	if err != nil {
		t.Fatal(err)
	}
	states := ledgerStatesIn(string(data))
	sawPublishedUnknown := false
	for _, st := range states {
		if st == "published_durability_unknown" {
			sawPublishedUnknown = true
		}
	}
	if !sawPublishedUnknown {
		t.Fatalf("ledger states = %v, want a durable published_durability_unknown record revoking retry permission", states)
	}

	// The consumer drains the retained artifact (removes the evidence), then
	// the delivery is retried. The next call must NOT re-apply over the
	// stale retryable-rejected record: published-unknown without retained
	// evidence stays uncertain, never re-applies.
	if err := os.Remove(curPath); err != nil {
		t.Fatal(err)
	}
	outcome2, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil {
		t.Fatalf("post-drain apply: %v", err)
	}
	if outcome2.State != LedgerUncertain {
		t.Fatalf("post-drain state = %q, want uncertain (no re-apply over proven publication)", outcome2.State)
	}
	newEntries, _ := os.ReadDir(fsq.AgentInboxNew(base, "claude"))
	curEntries, _ := os.ReadDir(fsq.AgentInboxCur(base, "claude"))
	if len(newEntries)+len(curEntries) != 0 {
		t.Fatalf("DUPLICATE: a re-apply published the payload again: new=%d cur=%d, want 0", len(newEntries), len(curEntries))
	}
}

// TestApplyWithLedgerRetryReArmsPreparedBeforeEvidenceLookup: the retry arm
// must durably append the fresh prepared intent BEFORE inspecting evidence.
// Observed ordering defect (review-827-r4): with evidence lookup first, a
// carrier-sync success followed by a failed/crashed commit append leaves the
// old rejected(retryable) record as the effective durable state — a later
// invocation after the artifact is consumed then re-applies. With re-arm
// first, the same crash leaves prepared, which never re-applies without
// evidence. Simulated here by re-arming against a ledger file whose appends
// fail: the invocation must do no recovery/apply work and must leave the
// last durable state as the original rejected record (not prepared, not
// committed), reporting the refusal.
func TestApplyWithLedgerRetryReArmsPreparedBeforeEvidenceLookup(t *testing.T) {
	root, ledger := newLedgerTestRoot(t)
	base := rootRootDir(t, root)
	env := testEnvelope([]byte("rearm-before-evidence"))

	if err := ledger.appendRecord(ledgerRecord{
		Version:       ledgerSchemaVersion,
		State:         LedgerRejected,
		SourceHost:    env.SourceHost,
		TransferID:    env.TransferID,
		PayloadSHA256: env.PayloadSHA256,
		Retryable:     true,
		Reason:        "apply failed (retryable): injected",
	}); err != nil {
		t.Fatal(err)
	}

	// The delivery has been retained (consumer drain): evidence exists.
	curPath := filepath.Join(fsq.AgentInboxCur(base, "claude"), TransferFilename(env.SourceHost, env.TransferID))
	if err := os.WriteFile(curPath, env.Payload, 0o600); err != nil {
		t.Fatal(err)
	}

	// Re-arm append fails (append-failure/crash simulation): the invocation
	// must stop before any evidence inspection or apply. With the defective
	// (evidence-first) ordering this invocation instead promotes via the
	// retained evidence and only fails when the commit append fails —
	// leaving rejected(retryable) as the last durable state, so a later
	// invocation after the artifact is consumed re-applies.
	root.SetAppendFaultForTest(func(dir, filename string, data []byte) (bool, error) {
		return false, errors.New("injected: ledger append failed")
	})
	t.Cleanup(func() { root.SetAppendFaultForTest(nil) })

	outcome, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if outcome.State != LedgerRejected || !strings.Contains(outcome.Reason, "re-arming prepared intent failed") {
		t.Fatalf("state = %q reason = %q, want the re-arm-failure refusal", outcome.State, outcome.Reason)
	}

	// The last durable record is still the original rejected(retryable):
	// nothing was applied, nothing was promoted.
	root.SetAppendFaultForTest(nil)
	data, err := os.ReadFile(filepath.Join(base, "bridge", "transfer-ledger", "mac_claude", ledgerRecordName(env.SourceHost, env.TransferID)))
	if err != nil {
		t.Fatal(err)
	}
	states := ledgerStatesIn(string(data))
	if len(states) != 1 || states[0] != "rejected" {
		t.Fatalf("ledger states = %v, want only the original rejected record (no partial re-arm, no apply)", states)
	}
	newEntries, _ := os.ReadDir(fsq.AgentInboxNew(base, "claude"))
	if len(newEntries) != 0 {
		t.Fatalf("applied during a failed re-arm: %d artifacts in new, want 0", len(newEntries))
	}

	// Recovery after the append heals: the retry now re-arms prepared first
	// and resolves through the standard prepared path.
	outcome2, err := ApplyWithLedger(ledger, root, "mac", "claude", env)
	if err != nil {
		t.Fatalf("retry after repair: %v", err)
	}
	if outcome2.State != LedgerCommitted {
		t.Fatalf("state after repair = %q (%s), want committed", outcome2.State, outcome2.Reason)
	}
}
