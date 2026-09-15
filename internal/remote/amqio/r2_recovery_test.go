package amqio

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// Regression tests for the Pro round-2 findings on the carrier
// (agent-message-queue-611.22.36 packets 5a 5b 6a 6b, and .37). Each fails
// against the carrier as it was before the fix.

func deliverCommand(t *testing.T, root, from, body string, hdr func(*format.Header)) string {
	t.Helper()
	now := time.Now()
	mid, err := format.NewMessageID(now)
	if err != nil {
		t.Fatal(err)
	}
	msg := format.Message{Header: format.Header{
		Schema: format.CurrentSchema, ID: mid, From: from, To: []string{DefaultHandle},
		Thread: "p2p/" + from + "__remote", Subject: "cmd", Created: now.UTC().Format(time.RFC3339Nano), Kind: "todo",
	}, Body: body}
	if hdr != nil {
		hdr(&msg.Header)
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	droot := mustOpenDeliveryRoot(t, root)
	if _, err := fsq.DeliverToInboxes(droot, []string{DefaultHandle}, mid+".md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	return mid
}

const sessionListBody = `{"schema":"amq.remote.command/1","op":"session.list"}`

func countFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return len(entries)
}

// TestNoReplyBeforeClaim reproduces agent-message-queue-611.22.37: the inline
// reply used to go out BEFORE the claim, so a failed claim left the command in
// new and the next tick answered it again under a fresh id. The claim and
// receipt now come first; a command whose claim fails has sent nothing, and
// once the claim succeeds the caller gets exactly one reply.
func TestNoReplyBeforeClaim(t *testing.T) {
	root, carrier, _, _ := newCarrierEnv(t)
	deliverCommand(t, root, "codex", sessionListBody, nil)
	cur := fsq.AgentInboxCur(root, DefaultHandle)
	if err := os.Chmod(cur, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cur, 0o700) })

	if _, err := carrier.ImportOnce(); err == nil {
		t.Fatal("tick 1: claim into a read-only cur succeeded unexpectedly")
	}
	if n := countFiles(t, fsq.AgentInboxNew(root, "codex")); n != 0 {
		t.Fatalf("tick 1: %d reply(ies) sent before the command was claimed (.37 — a failed claim re-answers under a fresh id)", n)
	}
	if n := countFiles(t, fsq.AgentInboxNew(root, DefaultHandle)); n != 1 {
		t.Fatalf("tick 1: command not left in new for retry: %d", n)
	}

	if err := os.Chmod(cur, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 2; i <= 3; i++ {
		if _, err := carrier.ImportOnce(); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
		if n := countFiles(t, fsq.AgentInboxNew(root, "codex")); n != 1 {
			t.Fatalf("tick %d: %d replies, want exactly 1", i, n)
		}
	}
}

// TestFirstTickAnswersNonRequestOpOnce reproduces packet 6b: the first
// ImportOnce ran the new scan and THEN the startup cur sweep, so a command it
// had just claimed was swept and answered a second time — for non-request ops
// with a null body.
func TestFirstTickAnswersNonRequestOpOnce(t *testing.T) {
	root, carrier, _, _ := newCarrierEnv(t)
	deliverCommand(t, root, "codex", sessionListBody, nil)
	for i := 1; i <= 2; i++ {
		if _, err := carrier.ImportOnce(); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
		entries, _ := os.ReadDir(fsq.AgentInboxNew(root, "codex"))
		if len(entries) != 1 {
			t.Fatalf("tick %d: %d replies to one session.list, want 1 (6b — the startup sweep answered a just-claimed command again)", i, len(entries))
		}
		msg, err := format.ReadMessageFile(filepath.Join(fsq.AgentInboxNew(root, "codex"), entries[0].Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(msg.Body) == "" || strings.TrimSpace(msg.Body) == "null" {
			t.Fatalf("tick %d: reply body is %q", i, msg.Body)
		}
	}
}

// TestRecoverySweepReportsFailedRead reproduces packet 6a: recoverOne returned
// nil for EVERY read error, so a cur entry behind a transient I/O failure was
// counted as recovered, curRecovered went true, and the entry was never
// revisited. A failed read now fails the sweep; the next tick retries.
func TestRecoverySweepReportsFailedRead(t *testing.T) {
	root, carrier, _, _ := newCarrierEnv(t)
	id := "11111111-1111-4111-8111-1111111111a6"
	body := `{"schema":"amq.remote.command/1","op":"request.submit","request_id":"` + id + `","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(time.Now().Add(time.Minute)) + `","input":{"text":"x"}}`
	simulateCrashBetweenClaimAndReceipt(t, root, id, body)
	entries, _ := os.ReadDir(fsq.AgentInboxCur(root, DefaultHandle))
	if len(entries) != 1 {
		t.Fatalf("setup: %d cur entries", len(entries))
	}
	curFile := filepath.Join(fsq.AgentInboxCur(root, DefaultHandle), entries[0].Name())
	if err := os.Chmod(curFile, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(curFile, 0o600) })

	if _, err := carrier.ImportOnce(); err == nil {
		t.Fatal("tick 1: an unreadable cur entry was counted as recovered (6a)")
	}
	carrier.mu.Lock()
	recovered := carrier.curRecovered
	carrier.mu.Unlock()
	if recovered {
		t.Fatal("tick 1: curRecovered set although an entry could not be read (6a — never revisited)")
	}
	if err := os.Chmod(curFile, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := carrier.ImportOnce(); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	receiptPath := filepath.Join(root, "agents", DefaultHandle, "receipts", entries[0].Name()[:len(entries[0].Name())-3]+"__"+DefaultHandle+"__"+receipt.StageDrained+".json")
	if _, err := os.Stat(receiptPath); err != nil {
		t.Fatalf("tick 2: entry not recovered after the read succeeded: %v", err)
	}
}

// TestRecoveryDoesNotReportStoreReadFailureAsOutcome reproduces the second
// half of packet 6a: a failed read of our own record was wrapped as a
// native_error refusal and DELIVERED as the caller's answer, and the entry was
// retired. The read failure now fails the sweep; the caller is told nothing.
func TestRecoveryDoesNotReportStoreReadFailureAsOutcome(t *testing.T) {
	root, carrier, _, store := newCarrierEnv(t)
	id := "11111111-1111-4111-8111-1111111111a7"
	body := `{"schema":"amq.remote.command/1","op":"request.submit","request_id":"` + id + `","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(time.Now().Add(time.Minute)) + `","input":{"text":"x"}}`
	simulateCrashBetweenClaimAndReceipt(t, root, id, body)
	_ = store
	// The record store becomes unreadable (a permissions blip, not a missing record).
	storeDir := filepath.Join(root, "extensions", "remote")
	if err := os.Chmod(storeDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(storeDir, 0o700) })
	if _, err := carrier.ImportOnce(); err == nil {
		t.Fatal("sweep succeeded although the record store is unreadable (6a)")
	}
	if n := countFiles(t, fsq.AgentInboxNew(root, "codex")); n != 0 {
		t.Fatalf("%d reply(ies) delivered for a record we could not read (6a — a store failure is not an outcome)", n)
	}
}

// TestRecoveryReplyRepairsCommittedDeliveryInPlace reproduces packet 5a: the
// recovery reply had no committed-delivery finalization, so a rename that
// succeeded with an unconfirmed directory fsync was reported as a failure and
// the next sweep delivered the reply again. The one shared tail now repairs
// the fsync in place and reports success.
func TestRecoveryReplyRepairsCommittedDeliveryInPlace(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"codex", "alice"} {
		if err := fsq.EnsureAgentDirs(root, h); err != nil {
			t.Fatal(err)
		}
	}
	dest := mustOpenDeliveryRoot(t, root)
	var mu sync.Mutex
	attempts := 0
	dest.SetSyncDirFaultForTest(func(dir string) error {
		if !strings.HasSuffix(dir, "new") {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if attempts == 1 {
			return errors.New("simulated fsync failure")
		}
		return nil
	})
	carrier := &Carrier{me: "alice", now: time.Now}
	origin := map[string]string{"from": "codex"}
	snap := protocol.Snapshot{State: protocol.StateCompleted, Epoch: "e", Revision: 3, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	created := time.Now().UTC().Format(time.RFC3339Nano)
	if err := carrier.replyWithRecovery(dest, origin, "recover", snap, nil, "cmd-5a", created); err != nil {
		t.Fatalf("recovery reply reported failure for a visible delivery: %v (5a)", err)
	}
	if n := countFiles(t, fsq.AgentInboxNew(root, "codex")); n != 1 {
		t.Fatalf("%d files, want 1", n)
	}
	mu.Lock()
	got := attempts
	mu.Unlock()
	if got < 2 {
		t.Fatalf("SyncDir attempted %d time(s), want a retry (5a)", got)
	}
}

// poisonSubmit is a command whose reply names a project the carrier has no
// router for, so its route is poison and the command goes to DLQ.
func poisonSubmit(t *testing.T, root string) string {
	t.Helper()
	body := `{"schema":"amq.remote.command/1","op":"request.submit","request_id":"11111111-1111-4111-8111-1111111111a8","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(time.Now().Add(time.Minute)) + `","input":{"text":"x"}}`
	return deliverCommand(t, root, "codex", body, func(h *format.Header) {
		h.ReplyProject = "nonexistent-project"
		h.ReplyTo = "codex@nowhere"
	})
}

func dlqReceiptPath(root, id string) string {
	return filepath.Join(root, "agents", DefaultHandle, "receipts", id+"__"+DefaultHandle+"__"+receipt.StageDLQ+".json")
}

// TestDLQReceiptIsOwedAfterFailedWrite reproduces packet 5b: the poison path
// moved the command to DLQ and then wrote the receipt; a failed receipt write
// left a consumed command with no receipt and nothing that would ever revisit
// it. The receipt is now an obligation retried on the next tick.
func TestDLQReceiptIsOwedAfterFailedWrite(t *testing.T) {
	root, carrier, _, _ := newCarrierEnv(t)
	id := poisonSubmit(t, root)
	receipts := filepath.Join(root, "agents", DefaultHandle, "receipts")
	if err := os.Chmod(receipts, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(receipts, 0o700) })

	_, _ = carrier.ImportOnce()
	if n := countFiles(t, fsq.AgentInboxNew(root, DefaultHandle)); n != 0 {
		t.Fatalf("poison command still in new: %d", n)
	}
	if _, err := os.Stat(dlqReceiptPath(root, id)); err == nil {
		t.Fatal("setup: receipt written although the directory is read-only")
	}
	if err := os.Chmod(receipts, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := carrier.ImportOnce(); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if _, err := os.Stat(dlqReceiptPath(root, id)); err != nil {
		t.Fatalf("tick 2: owed DLQ receipt never written (5b — the sender waits forever): %v", err)
	}
}

// TestDLQReceiptRebuiltAfterRestart is packet 5b across a restart: the owed
// set is in memory, so a new carrier over the same root rebuilds missing DLQ
// receipts from the DLQ entries on its startup sweep.
func TestDLQReceiptRebuiltAfterRestart(t *testing.T) {
	root, carrier, _, store := newCarrierEnv(t)
	id := poisonSubmit(t, root)
	if _, err := carrier.ImportOnce(); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if err := os.Remove(dlqReceiptPath(root, id)); err != nil {
		t.Fatalf("setup: %v", err)
	}
	restarted, _ := newCarrierOverExisting(t, root, store)
	if _, err := restarted.ImportOnce(); err != nil {
		t.Fatalf("restart tick: %v", err)
	}
	if _, err := os.Stat(dlqReceiptPath(root, id)); err != nil {
		t.Fatalf("DLQ receipt not rebuilt from the DLQ entry after restart (5b): %v", err)
	}
}
