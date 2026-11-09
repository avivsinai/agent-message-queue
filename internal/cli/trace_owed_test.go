package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
)

// Review of the owed-receipt trace: while the receipt directory still cannot
// be written (the reason a receipt is owed), trace skipped the agent before
// reading its owed receipts.
func TestTraceShowsAnOwedReceiptWhileTheReceiptsDirIsBroken(t *testing.T) {
	root := setupReceiptsTestRoot(t)
	configureSendTestRoot(t, root, "alice", "bob")
	sent := runSendJSONForTest(t, "--root", root, "--me", "bob", "--to", "alice", "--subject", "trace", "--body", "hello", "--json")
	id := sent["id"].(string)
	dir := filepath.Join(root, "agents", "alice", "receipts")
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ident, err := fsq.SnapshotDeliveryRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	r, err := fsq.OpenDeliveryRoot(root, ident)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if err := receipt.EmitOwed(r, receipt.New(id, "", "bob", "alice", receipt.StageDrained, "")); err == nil || !strings.Contains(err.Error(), "parked") {
		t.Fatalf("receipt not parked: %v", err)
	}
	out, _, _ := captureEnvOutput(t, func() error { return runTrace([]string{id, "--root", root, "--json"}) })
	if !strings.Contains(out, `"owed_receipt"`) {
		t.Fatalf("trace hid the owed receipt while receipts/ is broken: %s", out)
	}
}
