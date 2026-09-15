package amqio

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// TestDrainedReceiptEmittedWhenLedgerAnswerExists reproduces the gate finding
// on agent-message-queue-611.22.36 (union 42d923a4): the drained receipt write
// fails once AFTER the command was claimed into cur and its reply ledgered.
// Every later reconciliation pass took recoverOne's ledger-first early return
// and never emitted the receipt, so `send --wait-for drained` never completed.
func TestDrainedReceiptEmittedWhenLedgerAnswerExists(t *testing.T) {
	root, carrier, _, _ := newCarrierEnv(t)
	mid := deliverCommand(t, root, "codex", sessionListBody, nil)

	receiptsDir := fsq.AgentReceipts(root, DefaultHandle)
	if err := os.Chmod(receiptsDir, 0o500); err != nil {
		t.Fatal(err)
	}
	if _, err := carrier.ImportOnce(); err == nil {
		t.Fatal("expected the receipt write to fail while the receipts dir is read-only")
	}
	if err := os.Chmod(receiptsDir, 0o700); err != nil {
		t.Fatal(err)
	}

	if n := countFiles(t, fsq.AgentInboxNew(root, DefaultHandle)); n != 0 {
		t.Fatalf("command still in new: %d", n)
	}
	if n := countFiles(t, fsq.AgentInboxCur(root, DefaultHandle)); n != 1 {
		t.Fatalf("command not claimed into cur: %d", n)
	}

	// Steady-state reconciliation ticks.
	for i := 0; i < 3; i++ {
		if _, err := carrier.ImportOnce(); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}
	// Restart: a fresh carrier over the same root and endpoint has empty
	// in-memory state, so its first ImportOnce runs the full cur sweep.
	c2, err := New(root, DefaultHandle, carrier.ep)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c2.ImportOnce(); err != nil {
		t.Fatalf("restart import: %v", err)
	}

	if n := countFiles(t, fsq.AgentInboxNew(root, "codex")); n != 1 {
		t.Fatalf("caller got %d replies, want exactly 1", n)
	}
	want := filepath.Join(receiptsDir, mid+"__"+DefaultHandle+"__drained.json")
	if _, err := os.Stat(want); err != nil {
		names, _ := os.ReadDir(receiptsDir)
		got := []string{}
		for _, n := range names {
			got = append(got, n.Name())
		}
		t.Fatalf("drained receipt for the claimed command was never emitted: %v (receipts dir holds %v)", err, got)
	}
}
