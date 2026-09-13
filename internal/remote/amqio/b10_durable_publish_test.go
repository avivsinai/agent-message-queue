package amqio

import (
	"errors"
	"fmt"
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
// The republish is idempotent (B1): the message id is deterministic
// (publish__<ref>__rev<N>), so resolvePublishCollision returns nil on a
// byte-identical collision — no amplification.
//
// Acceptance:
//
//	(a) PublishedRevision does NOT advance while the fault is active.
//	(b) The publish error IS a CommittedDurabilityError (not a pre-rename
//	    ordinary error) — the file for the terminal revision IS visible.
//	(c) After the fault clears, Reconcile republishes the SAME revision and
//	    PublishedRevision advances.
//	(d) No amplification: multiple Reconcile ticks with the fault on produce
//	    exactly ONE file for the terminal revision.
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
	var publishErr error
	var carrier *Carrier
	ep := core.New(core.Config{
		Store: store,
		Publish: func(s protocol.Snapshot, origin map[string]string) error {
			publishErr = carrier.Publish(s, origin)
			return publishErr
		},
	})
	carrier, err = New(root, DefaultHandle, ep)
	if err != nil {
		t.Fatalf("carrier: %v", err)
	}
	// The fault fires only on the post-rename "new" dir sync (not the
	// pre-rename tmp sync), so the rename commits and the error is a
	// CommittedDurabilityError (B2).
	carrier.SetSyncDirFaultForTest(func(dir string) error {
		if faultActive && strings.HasSuffix(dir, "new") {
			return errors.New("simulated fsync failure")
		}
		return nil
	})
	rt := fake.New("fake", "e_1")
	ep.Register(rt)
	t.Cleanup(func() { _ = ep.Close() })

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
		t.Fatalf("(a) PublishedRevision=%d advanced to Revision=%d while the fault was active (B10)", rec.PublishedRevision, rec.Revision)
	}

	// (b) The publish error IS a CommittedDurabilityError (not a pre-rename
	// ordinary error). If the fault fired on the pre-rename tmp sync, the
	// error would be a plain error and the file would NOT be visible.
	var committed *fsq.CommittedDurabilityError
	if !errors.As(publishErr, &committed) {
		t.Fatalf("(b) publish error is %T: %v, want *fsq.CommittedDurabilityError (B2 — the fault must fire on the post-rename sync so the rename is visible but fsync is unknown)", publishErr, publishErr)
	}
	// The terminal revision's file IS visible (the rename committed).
	revLabel := fmt.Sprintf("revision:%d", rec.Revision)
	count := countFilesWithLabel(root, "codex", revLabel)
	if count != 1 {
		t.Fatalf("(b) expected 1 visible file for %s, got %d (B2 — the rename must have committed for a CommittedDurabilityError)", revLabel, count)
	}

	// (d) No amplification: run Reconcile 5 more times with the fault still
	// on. Each republish resolves to the same deterministic filename and
	// resolvePublishCollision returns nil. Exactly ONE file for the terminal
	// revision must exist — no flooding.
	for i := 0; i < 5; i++ {
		_ = ep.Reconcile()
	}
	count = countFilesWithLabel(root, "codex", revLabel)
	if count != 1 {
		t.Fatalf("(d) after 6 Reconcile ticks with fault on, expected 1 file for %s, got %d (B1 — deterministic id + resolvePublishCollision must make retries no-ops, no amplification)", revLabel, count)
	}

	// (c) Clear the fault and Reconcile. The same revision is republished and
	// PublishedRevision advances.
	faultActive = false
	if err := ep.Reconcile(); err != nil {
		t.Fatalf("(c) Reconcile: %v", err)
	}

	rec, _, _ = store.Get(key)
	if rec == nil {
		t.Fatal("record not found after reconcile")
	}
	if rec.PublishedRevision < rec.Revision {
		t.Fatalf("(c) PublishedRevision=%d did not advance to Revision=%d after the fault cleared (B10)", rec.PublishedRevision, rec.Revision)
	}
}

// countFilesWithLabel counts files in an agent's inbox/new whose body contains
// the given label string.
func countFilesWithLabel(root, handle, label string) int {
	dir := fsq.AgentInboxNew(root, handle)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	count := 0
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if strings.Contains(string(data), label) {
			count++
		}
	}
	return count
}

// TestB10InlineReplyToleratesCommittedDurabilityError reproduces B3: the
// tolerant/strict distinction has zero coverage. Mutation M4 flipped reply()
// to durabilityStrict (deleting the distinction entirely) and the whole
// package still passed. This test fails if the inline reply becomes strict.
//
// The inline reply path (durabilityTolerant) must treat a
// CommittedDurabilityError as success — the inline path has no retry, and
// making it fail would leave the command claimed with no answer. The known
// exposure (non-request op + power loss) is closed by cur recovery (#752).
func TestB10InlineReplyToleratesCommittedDurabilityError(t *testing.T) {
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
	carrier.SetSyncDirFaultForTest(func(dir string) error {
		if faultActive && strings.HasSuffix(dir, "new") {
			return errors.New("simulated fsync failure")
		}
		return nil
	})
	rt := fake.New("fake", "e_1")
	ep.Register(rt)
	t.Cleanup(func() { _ = ep.Close() })

	// Submit a command so the endpoint is ready.
	body := `{"schema":"amq.remote.command/1","op":"request.submit","request_id":"11111111-1111-4111-8111-111111111420","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(time.Now().Add(time.Minute)) + `","input":{"text":"work"}}`
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

	// Arm the fault. The next INLINE reply (a non-request op like
	// request.get) must TOLERATE the CommittedDurabilityError — the reply is
	// visible (rename committed) even though fsync is unknown. If the inline
	// path were strict, ImportOnce would return an error and the command
	// would stay in new with no answer.
	faultActive = true
	getBody := `{"schema":"amq.remote.command/1","op":"request.get","request_id":"11111111-1111-4111-8111-111111111420","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(time.Now().Add(time.Minute)) + `"}`
	now2 := time.Now()
	id2, _ := format.NewMessageID(now2)
	msg2 := format.Message{Header: format.Header{
		Schema: format.CurrentSchema, ID: id2, From: "codex", To: []string{DefaultHandle},
		Thread: "p2p/codex__remote", Subject: "get", Created: now2.UTC().Format(time.RFC3339Nano), Kind: "todo",
	}, Body: getBody}
	data2, _ := msg2.Marshal()
	identity2, _ := fsq.SnapshotDeliveryRoot(root)
	droot2, _ := fsq.OpenDeliveryRoot(root, identity2)
	if _, err := fsq.DeliverToInboxes(droot2, []string{DefaultHandle}, id2+".md", data2); err != nil {
		t.Fatalf("deliver get: %v", err)
	}
	_ = droot2.Close()

	// The inline reply (request.get) fires during ImportOnce. With the fault
	// on, the reply's DeliverToInboxes hits a CommittedDurabilityError. The
	// tolerant path treats it as success — ImportOnce returns n=1, no error.
	n, err = carrier.ImportOnce()
	if err != nil {
		t.Fatalf("inline reply did not tolerate CommittedDurabilityError (B3 — the inline path must be tolerant, not strict): %v", err)
	}
	if n < 1 {
		t.Fatalf("inline reply was not handled (B3): n=%d", n)
	}
	// The reply WAS visible (the rename committed despite fsync failure).
	if entries, _ := os.ReadDir(fsq.AgentInboxNew(root, "codex")); len(entries) == 0 {
		t.Fatal("B3: no reply visible — the inline reply did not deliver at all")
	}
}
