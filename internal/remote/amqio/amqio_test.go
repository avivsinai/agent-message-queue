package amqio

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// TestImportCommandAndPublishResult is the AMQ carrier happy path: a peer
// handle sends a request.submit as an ordinary message, the endpoint imports
// it, the message is claimed into cur with a receipt, and the running and
// completed revisions come back to the sender as messages in the same thread.
func TestImportCommandAndPublishResult(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"codex", DefaultHandle} {
		if err := fsq.EnsureAgentDirs(root, h); err != nil {
			t.Fatal(err)
		}
	}
	store, err := requests.Open(filepath.Join(root, "extensions", "remote"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	var carrier *Carrier
	ep := core.New(core.Config{Store: store, Publish: func(s protocol.Snapshot, origin map[string]string) error {
		return carrier.Publish(s, origin)
	}})
	carrier, err = New(root, DefaultHandle, ep)
	if err != nil {
		t.Fatalf("carrier: %v", err)
	}
	rt := fake.New("fake", "e_1")
	ep.Register(rt)
	t.Cleanup(func() { _ = ep.Close() })

	body := `{"schema":"amq.remote.command/1","op":"request.submit","request_id":"11111111-1111-4111-8111-111111111301","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(time.Now().Add(time.Minute)) + `","input":{"text":"review the diff"}}`
	now := time.Now()
	id, err := format.NewMessageID(now)
	if err != nil {
		t.Fatal(err)
	}
	msg := format.Message{Header: format.Header{Schema: format.CurrentSchema, ID: id, From: "codex", To: []string{DefaultHandle}, Thread: "p2p/codex__remote", Subject: "submit", Created: now.UTC().Format(time.RFC3339Nano), Kind: "todo"}, Body: body}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	identity, err := fsq.SnapshotDeliveryRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	droot, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fsq.DeliverToInboxes(droot, []string{DefaultHandle}, id+".md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	_ = droot.Close()

	n, err := carrier.ImportOnce()
	if err != nil || n != 1 {
		t.Fatalf("import: n=%d err=%v", n, err)
	}
	if entries, _ := os.ReadDir(fsq.AgentInboxNew(root, DefaultHandle)); len(entries) != 0 {
		t.Fatalf("command still in new: %d", len(entries))
	}
	if entries, _ := os.ReadDir(fsq.AgentInboxCur(root, DefaultHandle)); len(entries) != 1 {
		t.Fatalf("command not claimed into cur: %d", len(entries))
	}
	if entries, _ := os.ReadDir(fsq.AgentReceipts(root, DefaultHandle)); len(entries) != 1 {
		t.Fatalf("no drained receipt: %d", len(entries))
	}

	rt.Complete("11111111-1111-4111-8111-111111111301", "two defects")

	entries, err := os.ReadDir(fsq.AgentInboxNew(root, "codex"))
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]bool{}
	for _, e := range entries {
		m, err := format.ReadMessageFile(filepath.Join(fsq.AgentInboxNew(root, "codex"), e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if m.Header.Thread != "p2p/codex__remote" || !strings.HasPrefix(m.Header.Subject, subjectPrefix) {
			t.Fatalf("unexpected reply header: %+v", m.Header)
		}
		states[strings.TrimPrefix(m.Header.Subject, subjectPrefix)] = true
	}
	if !states["running"] || !states["completed"] {
		t.Fatalf("expected running and completed revisions, got %v", states)
	}
	rec, ok, err := store.Get(requests.Key{CreatorHost: "amq:codex", TargetID: "fake", RequestID: "11111111-1111-4111-8111-111111111301"})
	if err != nil || !ok {
		t.Fatalf("record: ok=%v err=%v", ok, err)
	}
	if rec.State != protocol.StateCompleted || rec.PublishedRevision != rec.Revision {
		t.Fatalf("record not completed and published: %+v", rec.Snapshot)
	}
}
