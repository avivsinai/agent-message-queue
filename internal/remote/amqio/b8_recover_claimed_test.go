package amqio

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// TestB8RecoverClaimedResendsReplyWhenOnlyReceiptLanded reproduces B8
// (agent-message-queue-611.22.35): recoverOne writes the receipt before the
// reply, so a claimed entry whose receipt exists may still owe its reply.
// recoverClaimed used to drop such an entry from the pending set on the
// strength of the receipt alone; the reply was never sent.
func TestB8RecoverClaimedResendsReplyWhenOnlyReceiptLanded(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{DefaultHandle, "codex"} {
		if err := fsq.EnsureAgentDirs(root, h); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	id, _ := format.NewMessageID(now)
	msg := format.Message{Header: format.Header{
		Schema: format.CurrentSchema, ID: id, From: "codex", To: []string{DefaultHandle},
		Thread: "p2p/codex__remote", Subject: "submit", Created: now.UTC().Format(time.RFC3339Nano), Kind: "todo",
	}, Body: `{"schema":"amq.remote.command/1","op":"request.submit","request_id":"11111111-1111-4111-8111-1111111111b8","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(now.Add(time.Minute)) + `","input":{"text":"work"}}`}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	curDir := filepath.Join("agents", DefaultHandle, "inbox", "cur")
	if err := os.WriteFile(filepath.Join(root, curDir, id+".md"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := requests.Open(filepath.Join(root, "extensions", "remote"))
	if err != nil {
		t.Fatal(err)
	}
	carrier, err := New(root, DefaultHandle, core.New(core.Config{Store: store}))
	if err != nil {
		t.Fatal(err)
	}
	identity, _ := fsq.SnapshotDeliveryRoot(root)
	droot, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = droot.Close() }()

	// The receipt landed; the process died before the reply.
	if err := receipt.EmitDeliveryRoot(droot, receipt.New(id, msg.Header.Thread, "codex", DefaultHandle, receipt.StageDrained, "claimed before crash")); err != nil {
		t.Fatal(err)
	}
	carrier.mu.Lock()
	carrier.claimedThisRun = map[string]claimedEntry{id: {filename: id + ".md", msgID: id}}
	carrier.mu.Unlock()

	if err := carrier.recoverClaimed(droot, map[string]claimedEntry{id: {filename: id + ".md", msgID: id}}); err != nil {
		t.Fatalf("recoverClaimed: %v", err)
	}
	entries, _ := os.ReadDir(fsq.AgentInboxNew(root, "codex"))
	if len(entries) != 1 {
		t.Fatalf("caller inbox has %d replies, want 1 (B8 — a receipt without a reply must re-send the recovery reply)", len(entries))
	}
	carrier.mu.Lock()
	_, pending := carrier.claimedThisRun[id]
	carrier.mu.Unlock()
	if pending {
		t.Fatal("entry still pending after a successful recovery")
	}
}
