package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
	uncertain, err := ledger.UncertainTransfers()
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
