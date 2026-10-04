package amqio

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// TestPublishUnderCommittedFsyncFaultDeliversOnce covers revision publication
// when the rename commits but the directory fsync fails (a
// CommittedDurabilityError), agent-message-queue-611.22.14 and .35 B12:
//
//	(a) PublishedRevision advances: the message is visible, and a visible
//	    message is published (re-delivery after consumption is never
//	    idempotent), so the fsync is repaired in place instead.
//	(b) Exactly one file for the terminal revision is visible.
//	(d) No amplification: Reconcile ticks with the fault still on leave one
//	    file, because the publish id is deterministic per (request, revision)
//	    and a byte-identical collision is a no-op (B1).
//	(e) The publish id starts with a timestamp, so it sorts chronologically
//	    against ordinary AMQ ids and drain --limit does not starve it.
func TestPublishUnderCommittedFsyncFaultDeliversOnce(t *testing.T) {
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

	key := requests.Key{CreatorHost: SourceHost(format.Header{From: "codex"}), TargetID: "fake", RequestID: "11111111-1111-4111-8111-111111111410"}
	rec, _, _ := store.Get(key)
	if rec == nil {
		t.Fatal("record not found after import")
	}

	// (a) B12: Activate the fault, then complete the run. The publish that
	// fires during the transition hits a CommittedDurabilityError (rename
	// succeeded, fsync failed). With B12, the carrier retries SyncDir in
	// place; on persistent failure it returns nil so PublishedRevision
	// advances — the message IS in the mailbox, re-delivery after
	// consumption is never idempotent.
	faultActive = true
	rt.Complete("11111111-1111-4111-8111-111111111410", "done")
	if err := ep.Tick(); err != nil {
		t.Fatalf("tick: %v", err)
	}

	rec, _, _ = store.Get(key)
	if rec == nil {
		t.Fatal("record not found after complete")
	}
	// B12: PublishedRevision advances even with the fault active — the
	// message is visible, durability is repaired in place.
	if rec.PublishedRevision < rec.Revision {
		t.Fatalf("(a) PublishedRevision=%d did not advance to Revision=%d (B12 — visible means published)", rec.PublishedRevision, rec.Revision)
	}

	// (b) The terminal revision's file IS visible (the rename committed).
	revLabel := fmt.Sprintf("revision:%d", rec.Revision)
	count := countFilesWithLabel(root, "codex", revLabel)
	if count != 1 {
		t.Fatalf("(b) expected 1 visible file for %s, got %d (the rename must have committed)", revLabel, count)
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
		t.Fatalf("(d) after 6 Reconcile ticks with fault on, expected 1 file for %s, got %d (deterministic id + resolvePublishCollision must make retries no-ops, no amplification)", revLabel, count)
	}

	// (e) The deterministic id must sort chronologically against ordinary AMQ
	// message ids. Assert the filename starts with a timestamp (not
	// "publish__"), so drain --limit 20 does not starve publish replies.
	for _, fn := range filesForLabel(root, "codex", revLabel) {
		if strings.HasPrefix(fn, "publish__") {
			t.Fatalf("(e) publish reply filename %q starts with 'publish__' — it must start with a timestamp so it sorts chronologically against ordinary AMQ ids", fn)
		}
		// Must start with a 4-digit year.
		if len(fn) < 4 || fn[:4] < "2000" {
			t.Fatalf("(e) publish reply filename %q does not start with a timestamp — it must sort chronologically", fn)
		}
	}
}

// countFilesWithLabel counts files in an agent's inbox/new whose body contains
// the given label string.
func countFilesWithLabel(root, handle, label string) int {
	return len(filesForLabel(root, handle, label))
}

// filesForLabel returns filenames in an agent's inbox/new whose body contains
// the given label string.
func filesForLabel(root, handle, label string) []string {
	dir := fsq.AgentInboxNew(root, handle)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if strings.Contains(string(data), label) {
			out = append(out, e.Name())
		}
	}
	return out
}

// TestB12CommittedDeliveryIsRepairedInPlace pins the one committed-delivery
// tail every reply goes through (agent-message-queue-611.22.35 B12; .36
// packet 5a for the recovery reply; B3 for the inline reply): on a
// CommittedDurabilityError the directory fsync is retried in place and the
// reply reports success, because the rename already made it visible. A
// persistent fsync failure still reports success and warns the operator.
func TestB12CommittedDeliveryIsRepairedInPlace(t *testing.T) {
	const created = "2026-09-14T11:00:00.000000Z"
	snap := protocol.Snapshot{State: protocol.StateCompleted, Epoch: "ep-b12", Revision: 42, RequestRef: "ref-b12", ObservedAt: created}
	origin := map[string]string{"from": "codex"} // same-project reply
	paths := map[string]func(c *Carrier, dest *fsq.DeliveryRoot) error{
		"publish": func(c *Carrier, dest *fsq.DeliveryRoot) error {
			return c.replyWith(dest, origin, "publish", snap, nil)
		},
		"recovery": func(c *Carrier, dest *fsq.DeliveryRoot) error {
			return c.replyWithRecovery(dest, origin, "recover", snap, nil, "cmd-b12", created)
		},
		"inline": func(c *Carrier, dest *fsq.DeliveryRoot) error {
			return c.answer(dest, origin, "cmd-b12", created, snap, nil)
		},
	}
	for name, send := range paths {
		for _, persistent := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/persistent=%v", name, persistent), func(t *testing.T) {
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
					if persistent || attempts == 1 {
						return errors.New("simulated fsync failure")
					}
					return nil
				})
				warned := false
				carrier := &Carrier{root: root, me: "alice", now: time.Now, Warn: func(error) { warned = true }}

				if err := send(carrier, dest); err != nil {
					t.Fatalf("reply reported failure for a visible delivery: %v", err)
				}
				if n := countFiles(t, fsq.AgentInboxNew(root, "codex")); n != 1 {
					t.Fatalf("%d visible replies, want 1", n)
				}
				mu.Lock()
				got := attempts
				mu.Unlock()
				if got < 2 {
					t.Fatalf("SyncDir attempted %d time(s), want a retry in place", got)
				}
				if warned != persistent {
					t.Fatalf("Warn fired=%v, want %v (warn only when every retry failed)", warned, persistent)
				}
			})
		}
	}
}

// TestB11TwoRevisionsBothLand verifies the B11 fix
// (agent-message-queue-611.22.35): recovery replies for different revisions
// of the same command produce different filenames (content-hash suffix), so
// both land in inbox/new without a collision. Previously the suffix was
// "noref" (identical for all revisions) -> resolvePublishCollision rejected
// the second as a non-byte-identical collision.
func TestB11TwoRevisionsBothLand(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	identity, _ := fsq.SnapshotDeliveryRoot(root)
	dest, _ := fsq.OpenDeliveryRoot(root, identity)

	carrier := &Carrier{me: "alice", now: time.Now}
	origin := map[string]string{"from": "codex", "thread": "p2p/alice__codex"}
	cmdMsgID := "2026-09-13T20-00-00.000000Z_pid1_b11cmd"
	msgCreated := "2026-09-13T20:00:00.000000Z"

	// First recovery: running snapshot (revision 1).
	snapRunning := protocol.Snapshot{
		State:      protocol.StateRunning,
		Epoch:      "ep-b11",
		Revision:   1,
		RequestRef: "ref-b11",
		ObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := carrier.replyWithRecovery(dest, origin, "recover", snapRunning, nil, cmdMsgID, msgCreated); err != nil {
		t.Fatalf("first recovery: %v", err)
	}

	// Second recovery: completed snapshot (revision 2).
	snapDone := protocol.Snapshot{
		State:      protocol.StateCompleted,
		Epoch:      "ep-b11",
		Revision:   2,
		RequestRef: "ref-b11",
		ObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := carrier.replyWithRecovery(dest, origin, "recover", snapDone, nil, cmdMsgID, msgCreated); err != nil {
		t.Fatalf("second recovery: %v", err)
	}

	// Both revisions must be visible.
	count1 := countFilesWithLabel(root, "codex", "revision:1")
	if count1 != 1 {
		t.Fatalf("expected 1 file for revision:1, got %d (B11 — different content must not collide)", count1)
	}
	count2 := countFilesWithLabel(root, "codex", "revision:2")
	if count2 != 1 {
		t.Fatalf("expected 1 file for revision:2, got %d (B11 — different content must not collide)", count2)
	}
}

// TestB17RecoveryReplyBodyIsNotEmpty verifies the #17 fix
// (agent-message-queue-611.22.35): replyWithRecovery must populate Body with
// the JSON-encoded response, not "". The bug was a `text, err :=` shadowing
// the outer `var text []byte` — the outer text stayed nil, Body was "".
// The JSON survived only in Context["remote"].
func TestB17RecoveryReplyBodyIsNotEmpty(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	identity, _ := fsq.SnapshotDeliveryRoot(root)
	dest, _ := fsq.OpenDeliveryRoot(root, identity)

	carrier := &Carrier{me: "alice", now: time.Now}
	origin := map[string]string{"from": "codex", "thread": "p2p/alice__codex"}
	cmdMsgID := "2026-09-14T11-00-00.000000Z_pid1_b17cmd"
	msgCreated := "2026-09-14T11:00:00.000000Z"

	snap := protocol.Snapshot{
		State:      protocol.StateCompleted,
		Epoch:      "ep-b17",
		Revision:   1,
		RequestRef: "ref-b17",
		ObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}

	if err := carrier.replyWithRecovery(dest, origin, "recover", snap, nil, cmdMsgID, msgCreated); err != nil {
		t.Fatalf("replyWithRecovery: %v", err)
	}

	// Read the delivered file and verify Body is not empty and decodes to
	// the expected snapshot.
	newDir := filepath.Join(root, "agents", "codex", "inbox", "new")
	entries, err := os.ReadDir(newDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 file in inbox/new, got %d", len(entries))
	}
	data, err := os.ReadFile(filepath.Join(newDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	msg, err := format.ParseMessage(data)
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	if msg.Body == "" {
		t.Fatal("Body is empty (B17 — shadowed text var left Body=\"\")")
	}
	// Body must decode to a protocol.Snapshot matching the input.
	var bodySnap protocol.Snapshot
	if err := json.Unmarshal([]byte(msg.Body), &bodySnap); err != nil {
		t.Fatalf("Body does not decode to Snapshot: %v (body=%q)", err, msg.Body)
	}
	if bodySnap.Revision != 1 || bodySnap.RequestRef != "ref-b17" {
		t.Fatalf("Body snapshot mismatch: %+v", bodySnap)
	}
	// Body must agree with Context["remote"].
	ctxRemote, ok := msg.Header.Context["remote"]
	if !ok {
		t.Fatal("Context[\"remote\"] is absent")
	}
	var ctxSnap protocol.Snapshot
	ctxBytes, _ := json.Marshal(ctxRemote)
	if err := json.Unmarshal(ctxBytes, &ctxSnap); err != nil {
		t.Fatalf("Context[\"remote\"] does not decode to Snapshot: %v", err)
	}
	if ctxSnap.Revision != bodySnap.Revision || ctxSnap.RequestRef != bodySnap.RequestRef || ctxSnap.State != bodySnap.State {
		t.Fatalf("Body (%+v) does not agree with Context[\"remote\"] (%+v)", bodySnap, ctxSnap)
	}
}
