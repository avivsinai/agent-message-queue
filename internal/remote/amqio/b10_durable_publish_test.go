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
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// TestB10DurablePublishPropagatesCommittedDurabilityError reproduces
// agent-message-queue-611.22.14: a CommittedDurabilityError (visible rename,
// unknown fsync) was converted to nil by reply(), so publishLocked advanced
// PublishedRevision and Reconcile never retried. A terminal result was lost
// on power loss with no retry.
//
// B10 fix: Publish uses durabilityStrict, which PROPAGATES the
// CommittedDurabilityError. publishLocked does not advance PublishedRevision,
// and Reconcile republishes the same immutable revision on the next tick.
// Receivers upsert by (request, revision), so a duplicate is a no-op.
//
// Acceptance criterion (the real path, not a direct replyWith call):
//
//	(a) PublishedRevision does NOT advance while the fault is active.
//	(b) After the fault clears, Reconcile republishes the SAME revision
//	    and PublishedRevision advances.
func TestB10DurablePublishPropagatesCommittedDurabilityError(t *testing.T) {
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

	var faultActive bool
	var carrier *Carrier
	ep := core.New(core.Config{
		Store: store,
		Publish: func(s protocol.Snapshot, origin map[string]string) error {
			return carrier.Publish(s, origin)
		},
	})
	carrier, err = New(root, DefaultHandle, ep)
	if err != nil {
		t.Fatalf("carrier: %v", err)
	}
	// Inject the sync fault into every DeliveryRoot Publish opens. The fault
	// fires only on the post-rename "new" dir sync (not the pre-rename tmp
	// sync), so the rename commits and the error is a CommittedDurabilityError.
	carrier.SetSyncDirFaultForTest(func(dir string) error {
		if faultActive && strings.HasSuffix(dir, "new") {
			return errors.New("simulated fsync failure")
		}
		return nil
	})
	rt := fake.New("fake", "e_1")
	ep.Register(rt)
	t.Cleanup(func() { _ = ep.Close() })

	// Submit a command — this creates a running record with revision=1.
	body := `{"schema":"amq.remote.command/1","op":"request.submit","request_id":"11111111-1111-4111-8111-111111111410","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(time.Now().Add(time.Minute)) + `","input":{"text":"work"}}`
	now := time.Now()
	id, _ := format.NewMessageID(now)
	msg := format.Message{Header: format.Header{
		Schema: format.CurrentSchema, ID: id, From: "codex", To: []string{DefaultHandle},
		Thread: "p2p/codex__remote", Subject: "submit", Created: now.UTC().Format(time.RFC3339Nano), Kind: "todo",
	}, Body: body}
	data, _ := msg.Marshal()
	identity, _ := fsq.SnapshotDeliveryRoot(root)
	droot, _ := fsq.OpenDeliveryRoot(root, identity)
	if _, err := fsq.DeliverToInboxes(droot, []string{DefaultHandle}, id+".md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	_ = droot.Close()

	n, err := carrier.ImportOnce()
	if err != nil || n != 1 {
		t.Fatalf("import: n=%d err=%v", n, err)
	}

	key := requests.Key{CreatorHost: "amq:codex", TargetID: "fake", RequestID: "11111111-1111-4111-8111-111111111410"}
	rec, _, _ := store.Get(key)
	if rec == nil {
		t.Fatal("record not found after import")
	}
	revisionBeforeComplete := rec.Revision

	// (a) Activate the fault, then complete the run. The publish that fires
	// during the transition must FAIL with CommittedDurabilityError, and
	// PublishedRevision must NOT advance.
	faultActive = true
	rt.Complete("11111111-1111-4111-8111-111111111410", "done")
	if err := ep.Tick(); err != nil {
		t.Fatalf("tick: %v", err)
	}

	rec, _, _ = store.Get(key)
	if rec == nil {
		t.Fatal("record not found after complete")
	}
	if rec.PublishedRevision >= rec.Revision {
		t.Fatalf("(a) PublishedRevision=%d advanced to Revision=%d while the fault was active (B10 — the CommittedDurabilityError must propagate so publishLocked does not advance)", rec.PublishedRevision, rec.Revision)
	}
	if rec.Revision <= revisionBeforeComplete {
		t.Fatalf("revision did not advance after complete: %d -> %d", revisionBeforeComplete, rec.Revision)
	}

	// Verify the message WAS visible (the rename committed) — the fault is on
	// fsync, not the rename. This proves the receiver saw it but we still
	// refused to call it "published."
	if entries, _ := os.ReadDir(fsq.AgentInboxNew(root, "codex")); len(entries) == 0 {
		t.Fatal("(a) no reply message visible — the rename did not commit, so this is not a CommittedDurabilityError scenario")
	}

	// (b) Clear the fault and Reconcile. The same revision is republished and
	// PublishedRevision advances.
	faultActive = false
	if err := ep.Reconcile(); err != nil {
		t.Fatalf("(b) Reconcile: %v", err)
	}

	rec, _, _ = store.Get(key)
	if rec == nil {
		t.Fatal("record not found after reconcile")
	}
	if rec.PublishedRevision < rec.Revision {
		t.Fatalf("(b) PublishedRevision=%d did not advance to Revision=%d after the fault cleared (B10 — Reconcile must republish the same revision)", rec.PublishedRevision, rec.Revision)
	}
}
