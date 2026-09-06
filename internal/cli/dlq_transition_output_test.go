package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
)

func TestRunDrainCorruptMessageWithoutSenderDoesNotWarn(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	const id = "corrupt-without-sender"
	deliverInvalidDLQTransitionFixture(t, root, "alice", id)

	stdout, stderr, err := captureEnvOutput(t, func() error {
		return runDrain([]string{"--root", root, "--me", "alice", "--json"})
	})
	if err != nil {
		t.Fatalf("drain corrupt senderless message: %v", err)
	}
	if strings.Contains(stderr, "receipt sender normalization failed") {
		t.Fatalf("senderless corrupt message emitted unactionable warning: %s", stderr)
	}
	var result drainResult
	if decodeErr := unmarshalJSONOutput(stdout, &result); decodeErr != nil {
		t.Fatalf("unmarshal senderless corrupt drain output: %v (output: %s)", decodeErr, stdout)
	}
	assertCompletedDLQResult(t, result.Drained, result.Count, id)
	assertCompletedDLQState(t, root, id)
	receipts, listErr := receipt.List(root, "alice", receipt.ListFilter{
		MsgID: id,
		Stage: receipt.StageDLQ,
	})
	if listErr != nil || len(receipts) != 1 {
		t.Fatalf("senderless corrupt receipts = %#v, err=%v; want one", receipts, listErr)
	}
	gotReceipt := receipts[0]
	if gotReceipt.Stage != receipt.StageDLQ || gotReceipt.Sender != "" {
		t.Fatalf("senderless corrupt receipt = %#v, want stage dlq and empty sender", gotReceipt)
	}
	if !strings.Contains(gotReceipt.Detail, "missing frontmatter") ||
		!strings.Contains(gotReceipt.Detail, receiptSenderUnavailableDetail) {
		t.Fatalf("senderless corrupt receipt detail = %q, want parse error plus sender explanation", gotReceipt.Detail)
	}
}

func assertCompletedDLQResult(t *testing.T, items []inboxItem, count int, id string) {
	t.Helper()
	if count != 1 || len(items) != 1 {
		t.Fatalf("completed DLQ output count = %d items = %#v, want 1", count, items)
	}
	item := items[0]
	if item.ID != id || item.ParseError == "" || !item.MovedToDLQ || item.MovedToCur {
		t.Fatalf("completed DLQ item = %#v", item)
	}
}

func assertCompletedDLQState(t *testing.T, root, id string) string {
	t.Helper()
	for _, path := range []string{
		filepath.Join(fsq.AgentInboxNew(root, "alice"), id+".md"),
		filepath.Join(fsq.AgentInboxCur(root, "alice"), id+".md"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("completed DLQ source remains at %s: %v", path, err)
		}
	}
	entries, err := os.ReadDir(fsq.AgentDLQNew(root, "alice"))
	if err != nil {
		t.Fatalf("read completed DLQ directory: %v", err)
	}
	if len(entries) != 1 || entries[0].IsDir() {
		t.Fatalf("completed DLQ entries = %#v, want one envelope", entries)
	}
	dlqPath := filepath.Join(fsq.AgentDLQNew(root, "alice"), entries[0].Name())
	env, original, err := fsq.ReadDLQEnvelopePath(dlqPath)
	if err != nil {
		t.Fatalf("read completed DLQ envelope: %v", err)
	}
	if env.OriginalID != id || string(original) != "missing frontmatter" {
		t.Fatalf("completed DLQ envelope = %#v body=%q, want original %q", env, original, id)
	}
	assertReceiptCount(t, root, "alice", id, receipt.StageDLQ, 1)
	return dlqPath
}

func deliverInvalidDLQTransitionFixture(t *testing.T, root, agent, id string) {
	t.Helper()
	if err := deliverInvalidDLQTransitionFixtureError(root, agent, id); err != nil {
		t.Fatalf("deliver invalid fixture: %v", err)
	}
}

func deliverInvalidDLQTransitionFixtureError(root, agent, id string) error {
	identity, err := fsq.SnapshotDeliveryRoot(root)
	if err != nil {
		return err
	}
	deliveryRoot, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		return err
	}
	defer func() { _ = deliveryRoot.Close() }()
	_, err = fsq.DeliverToInbox(deliveryRoot, agent, id+".md", []byte("missing frontmatter"))
	return err
}

func assertReceiptCount(t *testing.T, root, agent, id, stage string, want int) {
	t.Helper()
	receipts, err := receipt.List(root, agent, receipt.ListFilter{MsgID: id, Stage: stage})
	if err != nil {
		t.Fatalf("list %s receipts: %v", stage, err)
	}
	if len(receipts) != want {
		t.Fatalf("%s receipts for %s = %d, want %d", stage, id, len(receipts), want)
	}
}
