package amqio

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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
