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

// TestImportLeavesCommandOnPlainError proves the carrier leaves a command in
// new (no claim, no drained receipt) when Handle returns a plain, non-Refusal
// error, so a command whose record may not exist is retried rather than lost.
func TestImportLeavesCommandOnPlainError(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"codex", DefaultHandle} {
		if err := fsq.EnsureAgentDirs(root, h); err != nil {
			t.Fatal(err)
		}
	}
	stateDir := filepath.Join(root, "extensions", "remote")
	store, err := requests.Open(stateDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	var carrier *Carrier
	ep := core.New(core.Config{Store: store, Publish: func(s protocol.Snapshot, o map[string]string) error { return carrier.Publish(s, o) }})
	carrier, err = New(root, DefaultHandle, ep)
	if err != nil {
		t.Fatal(err)
	}
	rt := fake.New("fake", "e_1")
	ep.Register(rt)
	t.Cleanup(func() { _ = ep.Close() })

	// Deliver a valid submit command by mail.
	body := `{"schema":"amq.remote.command/1","op":"request.submit","request_id":"11111111-1111-4111-8111-1111111111e1","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(time.Now().Add(time.Minute)) + `","input":{"text":"x"}}`
	now := time.Now()
	id, _ := format.NewMessageID(now)
	msg := format.Message{Header: format.Header{Schema: format.CurrentSchema, ID: id, From: "codex", To: []string{DefaultHandle}, Thread: "p2p/codex__remote", Subject: "submit", Created: now.UTC().Format(time.RFC3339Nano), Kind: "todo"}, Body: body}
	data, _ := msg.Marshal()
	identity, _ := fsq.SnapshotDeliveryRoot(root)
	droot, _ := fsq.OpenDeliveryRoot(root, identity)
	if _, err := fsq.DeliverToInboxes(droot, []string{DefaultHandle}, id+".md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	_ = droot.Close()

	// Corrupt the on-disk record for this exact key so submit's store.Get
	// fails to decode it and returns a plain (non-Refusal) error, the class
	// the review found the carrier would wrongly drain.
	hostDir := filepath.Join(stateDir, "v1", "requests", "amq:codex")
	if err := os.MkdirAll(hostDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostDir, "fake__11111111-1111-4111-8111-1111111111e1.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := carrier.ImportOnce(); err != nil {
		t.Fatalf("import returned error: %v", err)
	}
	// The command must remain in new for retry; no record, no drained receipt.
	if entries, _ := os.ReadDir(fsq.AgentInboxNew(root, DefaultHandle)); len(entries) != 1 {
		t.Fatalf("command drained despite a plain store error: new has %d", len(entries))
	}
	if entries, _ := os.ReadDir(fsq.AgentReceipts(root, DefaultHandle)); len(entries) != 0 {
		t.Fatalf("drained receipt emitted despite no record: %d", len(entries))
	}
}

// deliverSubmit is a test helper: it delivers a request.submit for id with the
// given text into the endpoint handle's inbox.
func deliverSubmit(t *testing.T, root, id, text string) {
	t.Helper()
	body := `{"schema":"amq.remote.command/1","op":"request.submit","request_id":"` + id + `","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(time.Now().Add(time.Minute)) + `","input":{"text":"` + text + `"}}`
	now := time.Now()
	mid, _ := format.NewMessageID(now)
	msg := format.Message{Header: format.Header{Schema: format.CurrentSchema, ID: mid, From: "codex", To: []string{DefaultHandle}, Thread: "p2p/codex__remote", Subject: "submit", Created: now.UTC().Format(time.RFC3339Nano), Kind: "todo"}, Body: body}
	data, _ := msg.Marshal()
	identity, _ := fsq.SnapshotDeliveryRoot(root)
	droot, _ := fsq.OpenDeliveryRoot(root, identity)
	if _, err := fsq.DeliverToInboxes(droot, []string{DefaultHandle}, mid+".md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	_ = droot.Close()
}

// TestImportConflictRepliesToSender reproduces Pro B09's suppressed-reply hole:
// a request op that yields an op-specific Outcome (request_conflict) does not
// travel as a published revision, so the carrier must still reply to the sender
// with that outcome rather than silently claiming the command.
func TestImportConflictRepliesToSender(t *testing.T) {
	root := t.TempDir()
	_ = fsq.EnsureRootDirs(root)
	for _, h := range []string{"codex", DefaultHandle} {
		_ = fsq.EnsureAgentDirs(root, h)
	}
	store, err := requests.Open(filepath.Join(root, "extensions", "remote"))
	if err != nil {
		t.Fatal(err)
	}
	var carrier *Carrier
	ep := core.New(core.Config{Store: store, Publish: func(s protocol.Snapshot, o map[string]string) error { return carrier.Publish(s, o) }})
	carrier, err = New(root, DefaultHandle, ep)
	if err != nil {
		t.Fatal(err)
	}
	ep.Register(fake.New("fake", "e_1"))
	t.Cleanup(func() { _ = ep.Close() })

	id := "11111111-1111-4111-8111-1111111111c9"
	deliverSubmit(t, root, id, "first")
	if _, err := carrier.ImportOnce(); err != nil {
		t.Fatal(err)
	}
	// clear the sender's inbox of the running-revision replies so we isolate the conflict reply
	firstReplies, _ := os.ReadDir(fsq.AgentInboxNew(root, "codex"))
	baseline := len(firstReplies)

	deliverSubmit(t, root, id, "different bytes")
	if _, err := carrier.ImportOnce(); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(fsq.AgentInboxNew(root, "codex"))
	sawConflict := false
	for _, e := range entries {
		m, err := format.ReadMessageFile(filepath.Join(fsq.AgentInboxNew(root, "codex"), e.Name()))
		if err != nil {
			continue
		}
		if strings.Contains(m.Body, string(protocol.CodeRequestConflict)) {
			sawConflict = true
		}
	}
	if len(entries) <= baseline || !sawConflict {
		t.Fatalf("conflicting submit did not reply to sender with the outcome (entries %d, baseline %d, sawConflict %v)", len(entries), baseline, sawConflict)
	}
}

// TestImportNoopCancelRepliesToSender pins Pro/amit-pi's Q4 finding: a no-op
// terminal cancel carries an Outcome disposition with an empty Code and never
// publishes a revision, so the carrier must still reply to the sender.
func TestImportNoopCancelRepliesToSender(t *testing.T) {
	root := t.TempDir()
	_ = fsq.EnsureRootDirs(root)
	for _, h := range []string{"codex", DefaultHandle} {
		_ = fsq.EnsureAgentDirs(root, h)
	}
	store, err := requests.Open(filepath.Join(root, "extensions", "remote"))
	if err != nil {
		t.Fatal(err)
	}
	var carrier *Carrier
	ep := core.New(core.Config{Store: store, Publish: func(s protocol.Snapshot, o map[string]string) error { return carrier.Publish(s, o) }})
	carrier, err = New(root, DefaultHandle, ep)
	if err != nil {
		t.Fatal(err)
	}
	rt := fake.New("fake", "e_1")
	ep.Register(rt)
	t.Cleanup(func() { _ = ep.Close() })

	id := "11111111-1111-4111-8111-1111111111ca"
	deliverSubmit(t, root, id, "work")
	if _, err := carrier.ImportOnce(); err != nil {
		t.Fatal(err)
	}
	rt.Complete(id, "done") // record is now terminal
	baseline, _ := os.ReadDir(fsq.AgentInboxNew(root, "codex"))

	// Deliver a cancel for the already-terminal record -> noop_already_terminal.
	ref := protocol.EncodeRef("amq:codex", "fake", id)
	body := `{"schema":"amq.remote.command/1","op":"request.cancel","request_ref":"` + ref + `","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(time.Now().Add(time.Minute)) + `"}`
	now := time.Now()
	mid, _ := format.NewMessageID(now)
	msg := format.Message{Header: format.Header{Schema: format.CurrentSchema, ID: mid, From: "codex", To: []string{DefaultHandle}, Thread: "p2p/codex__remote", Subject: "cancel", Created: now.UTC().Format(time.RFC3339Nano), Kind: "todo"}, Body: body}
	data, _ := msg.Marshal()
	identity, _ := fsq.SnapshotDeliveryRoot(root)
	droot, _ := fsq.OpenDeliveryRoot(root, identity)
	if _, err := fsq.DeliverToInboxes(droot, []string{DefaultHandle}, mid+".md", data); err != nil {
		t.Fatal(err)
	}
	_ = droot.Close()
	if _, err := carrier.ImportOnce(); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(fsq.AgentInboxNew(root, "codex"))
	sawNoop := false
	for _, e := range entries {
		m, err := format.ReadMessageFile(filepath.Join(fsq.AgentInboxNew(root, "codex"), e.Name()))
		if err != nil {
			continue
		}
		if strings.Contains(m.Body, string(protocol.CancelNoopTerminal)) {
			sawNoop = true
		}
	}
	if len(entries) <= len(baseline) || !sawNoop {
		t.Fatalf("no-op terminal cancel did not reply to sender (entries %d, baseline %d, sawNoop %v)", len(entries), len(baseline), sawNoop)
	}
}

// TestImportStoreRefusalLeavesCommandInNew reproduces the blocker inside
// agent-message-queue-611.22.13 (B09 carrier): the carrier classified
// refusals by TYPE (errors.As *protocol.Refusal), but the store itself
// refuses with storage_full (ENOSPC, EPERM, oversize) and store_closed
// (shutdown). On every op EXCEPT submit-create those travel as an ERROR from
// Handle rather than as a Reply Outcome, so the previous Outcome.Code guard
// never saw them: the command was answered "refused" and CLAIMED. A cancel
// whose tombstone could not be written was consumed, and the later submit
// then executed the request the caller had cancelled. A store refusal must
// leave the command in inbox/new for the next import.
func TestImportStoreRefusalLeavesCommandInNew(t *testing.T) {
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
	ep.Register(fake.New("fake", "e_1"))
	t.Cleanup(func() { _ = ep.Close() })

	// A cancel for a request that was never submitted: the endpoint answers by
	// writing a cancel-before-submit tombstone, so the store write IS the
	// operation, and its refusal arrives as an error from Handle.
	reqID := "11111111-1111-4111-8111-111111111390"
	ref := protocol.EncodeRef("amq:codex", "fake", reqID)
	body := `{"schema":"amq.remote.command/1","op":"request.cancel","request_ref":"` + ref + `","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(time.Now().Add(time.Minute)) + `"}`
	now := time.Now()
	id, err := format.NewMessageID(now)
	if err != nil {
		t.Fatal(err)
	}
	msg := format.Message{Header: format.Header{
		Schema: format.CurrentSchema, ID: id, From: "codex", To: []string{DefaultHandle},
		Thread: "p2p/codex__remote", Subject: "cancel", Created: now.UTC().Format(time.RFC3339Nano), Kind: "todo",
	}, Body: body}
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

	// Force every store write to refuse with storage_full, exactly as a full
	// disk does. The refusal is a *protocol.Refusal, so the old type-based
	// classification accepted it as a durable answer and claimed the command.
	saved := requests.MaxRecordBytes
	requests.MaxRecordBytes = 1
	t.Cleanup(func() { requests.MaxRecordBytes = saved })

	if _, err := carrier.ImportOnce(); err != nil {
		t.Fatalf("import: %v", err)
	}
	if entries, _ := os.ReadDir(fsq.AgentInboxNew(root, DefaultHandle)); len(entries) != 1 {
		t.Fatalf("storage-refused cancel not left in new: %d", len(entries))
	}
	if entries, _ := os.ReadDir(fsq.AgentInboxCur(root, DefaultHandle)); len(entries) != 0 {
		t.Fatalf("storage-refused cancel was CLAIMED: %d (the cancelled request would later execute)", len(entries))
	}

	// With storage restored the retry succeeds and the command is claimed.
	requests.MaxRecordBytes = saved
	if _, err := carrier.ImportOnce(); err != nil {
		t.Fatalf("retry import: %v", err)
	}
	if entries, _ := os.ReadDir(fsq.AgentInboxNew(root, DefaultHandle)); len(entries) != 0 {
		t.Fatalf("cancel still in new after storage recovered: %d", len(entries))
	}
	if entries, _ := os.ReadDir(fsq.AgentInboxCur(root, DefaultHandle)); len(entries) != 1 {
		t.Fatalf("recovered cancel not claimed: %d", len(entries))
	}
}

// TestImportCrossProjectRepliesToCallerRoot reproduces
// agent-message-queue-611.22.30 (amqio wrong root): the carrier captured
// reply_to/reply_project in origin and never used them, delivering the reply
// and every published revision with fsq.DeliverToInboxes(OUR root, ...). A
// caller in another project therefore received nothing — its reply landed in
// a same-named mailbox inside the endpoint's own root (which MkdirAll happily
// created) while its command was claimed. That is the two-host case the
// subsystem exists for.
func TestImportCrossProjectRepliesToCallerRoot(t *testing.T) {
	endpointRoot := t.TempDir()
	callerRoot := t.TempDir()
	for root, handles := range map[string][]string{
		endpointRoot: {DefaultHandle},
		callerRoot:   {"codex"},
	} {
		if err := fsq.EnsureRootDirs(root); err != nil {
			t.Fatal(err)
		}
		for _, h := range handles {
			if err := fsq.EnsureAgentDirs(root, h); err != nil {
				t.Fatal(err)
			}
		}
	}

	store, err := requests.Open(filepath.Join(endpointRoot, "extensions", "remote"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	var carrier *Carrier
	ep := core.New(core.Config{Store: store, Publish: func(s protocol.Snapshot, origin map[string]string) error {
		return carrier.Publish(s, origin)
	}})
	carrier, err = New(endpointRoot, DefaultHandle, ep)
	if err != nil {
		t.Fatalf("carrier: %v", err)
	}
	// The injected contract stands in for cli.ResolveReplyRoute: the carrier
	// knows nothing about .amqrc or peer maps.
	var routedProject, routedReplyTo string
	carrier.SetReplyRouter(func(replyProject, replyTo string) (string, string, error) {
		routedProject, routedReplyTo = replyProject, replyTo
		if replyProject != "caller-project" {
			return "", "", errors.New("unknown peer project " + replyProject)
		}
		return callerRoot, "codex", nil
	})
	ep.Register(fake.New("fake", "e_1"))
	t.Cleanup(func() { _ = ep.Close() })

	body := `{"schema":"amq.remote.command/1","op":"request.submit","request_id":"11111111-1111-4111-8111-111111111330","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(time.Now().Add(time.Minute)) + `","input":{"text":"cross-project work"}}`
	now := time.Now()
	id, err := format.NewMessageID(now)
	if err != nil {
		t.Fatal(err)
	}
	msg := format.Message{Header: format.Header{
		Schema: format.CurrentSchema, ID: id, From: "codex", To: []string{DefaultHandle},
		Thread: "p2p/codex__remote", Subject: "submit", Created: now.UTC().Format(time.RFC3339Nano), Kind: "todo",
		FromProject: "caller-project", ReplyTo: "codex@session1", ReplyProject: "caller-project",
	}, Body: body}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	identity, err := fsq.SnapshotDeliveryRoot(endpointRoot)
	if err != nil {
		t.Fatal(err)
	}
	droot, err := fsq.OpenDeliveryRoot(endpointRoot, identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fsq.DeliverToInboxes(droot, []string{DefaultHandle}, id+".md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	_ = droot.Close()

	if _, err := carrier.ImportOnce(); err != nil {
		t.Fatalf("import: %v", err)
	}

	if routedProject != "caller-project" || routedReplyTo != "codex@session1" {
		t.Fatalf("router called with (%q, %q), want (caller-project, codex@session1)", routedProject, routedReplyTo)
	}
	// The reply and every published revision must land in the CALLER's root.
	callerInbox, _ := os.ReadDir(fsq.AgentInboxNew(callerRoot, "codex"))
	if len(callerInbox) == 0 {
		t.Fatal("caller received nothing in its own root")
	}
	// ...and nothing may be written into a codex mailbox inside OUR root.
	if entries, err := os.ReadDir(fsq.AgentInboxNew(endpointRoot, "codex")); err == nil && len(entries) > 0 {
		t.Fatalf("reply written into the endpoint's own root: %d message(s)", len(entries))
	}
}

// TestImportCrossProjectUnroutableLeavesCommandInNew pins the refusal half of
// agent-message-queue-611.22.30: when the reply cannot be routed, the command
// must stay in new (answerable later) rather than be claimed with its reply
// delivered into the endpoint's own root.
// TestImportCrossProjectUnroutableDoesNotBlockRoutable reproduces Pro B1+B3:
// an unroutable cross-project command must NOT stop a second, routable command
// from being handled in the same scan. F2: the test must make importOne return
// a REAL error (not a DLQ'd true), so errors.Join/never-abort is actually
// exercised. A poison message is DLQ'd (true, nil) — that does not test D1.
// Instead, message A is a same-project submit whose reply delivery FAILS
// (read-only peer inbox), producing (false, err). Message B is a routable
// same-project submit that sorts after A. If D1 is reverted (return on first
// error), B is never processed.
func TestImportCrossProjectUnroutableDoesNotBlockRoutable(t *testing.T) {
	endpointRoot := t.TempDir()
	if err := fsq.EnsureRootDirs(endpointRoot); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(endpointRoot, DefaultHandle); err != nil {
		t.Fatal(err)
	}
	// Create a "codex" mailbox, then make it read-only so the reply delivery
	// fails (a real error, not a DLQ).
	if err := fsq.EnsureAgentDirs(endpointRoot, "codex"); err != nil {
		t.Fatal(err)
	}
	codexInbox := filepath.Join(endpointRoot, "agents", "codex", "inbox", "new")
	if err := os.Chmod(codexInbox, 0500); err != nil { // read+execute, no write
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(codexInbox, 0700) })

	store, err := requests.Open(filepath.Join(endpointRoot, "extensions", "remote"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	var carrier *Carrier
	ep := core.New(core.Config{Store: store, Publish: func(s protocol.Snapshot, origin map[string]string) error {
		return carrier.Publish(s, origin)
	}})
	carrier, err = New(endpointRoot, DefaultHandle, ep)
	if err != nil {
		t.Fatalf("carrier: %v", err)
	}
	ep.Register(fake.New("fake", "e_1"))
	t.Cleanup(func() { _ = ep.Close() })

	identity, _ := fsq.SnapshotDeliveryRoot(endpointRoot)
	droot, _ := fsq.OpenDeliveryRoot(endpointRoot, identity)
	defer func() { _ = droot.Close() }()

	// Message A: same-project submit (from codex, no reply_project). The reply
	// delivery to codex/inbox/new fails (read-only). importOne returns
	// (false, err). Use an EARLIER message id so it sorts first.
	now := time.Now()
	idA, _ := format.NewMessageID(now.Add(-time.Second))
	bodyA := `{"schema":"amq.remote.command/1","op":"request.get","request_id":"11111111-1111-4111-8111-111111111331","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(time.Now().Add(time.Minute)) + `"}`
	msgA := format.Message{Header: format.Header{
		Schema: format.CurrentSchema, ID: idA, From: "codex", To: []string{DefaultHandle},
		Thread: "p2p/codex__remote", Subject: "submit", Created: now.UTC().Format(time.RFC3339Nano), Kind: "todo",
	}, Body: bodyA}
	dataA, _ := msgA.Marshal()
	if _, err := fsq.DeliverToInboxes(droot, []string{DefaultHandle}, idA+".md", dataA); err != nil {
		t.Fatalf("deliver A: %v", err)
	}

	// Message B: same-project submit (from a DIFFERENT handle whose inbox IS
	// writable, so its reply succeeds). B sorts after A.
	idB, _ := format.NewMessageID(now)
	if err := fsq.EnsureAgentDirs(endpointRoot, "other"); err != nil {
		t.Fatal(err)
	}
	bodyB := `{"schema":"amq.remote.command/1","op":"request.submit","request_id":"11111111-1111-4111-8111-111111111332","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(time.Now().Add(time.Minute)) + `","input":{"text":"B"}}`
	msgB := format.Message{Header: format.Header{
		Schema: format.CurrentSchema, ID: idB, From: "other", To: []string{DefaultHandle},
		Thread: "p2p/other__remote", Subject: "submit", Created: now.UTC().Format(time.RFC3339Nano), Kind: "todo",
	}, Body: bodyB}
	dataB, _ := msgB.Marshal()
	if _, err := fsq.DeliverToInboxes(droot, []string{DefaultHandle}, idB+".md", dataB); err != nil {
		t.Fatalf("deliver B: %v", err)
	}

	// ImportOnce: A's reply fails (real error), but B must still be processed.
	// If D1 is reverted (return on first error), B is left in new.
	n, _ := carrier.ImportOnce()
	_ = n
	// B was claimed into cur despite A's error.
	if entries, _ := os.ReadDir(fsq.AgentInboxCur(endpointRoot, DefaultHandle)); len(entries) < 1 {
		t.Fatalf("routable B not claimed (D1 reverted — loop aborted on A error): %d in cur", len(entries))
	}
	// A is still in new (reply failed, not claimed).
	if entries, _ := os.ReadDir(fsq.AgentInboxNew(endpointRoot, DefaultHandle)); len(entries) < 1 {
		t.Fatalf("A should still be in new (reply failed): %d", len(entries))
	}
}

// TestImportSameProjectEmptyFromStillWorks reproduces Pro B2: the pre-fix code
// tolerated a missing or unusable From on a same-project caller; the PR
// regressed it to an error that propagated through B1 and deadlocked the
// endpoint. D2 restores the tolerant handling — same-project with from:""
// is handled normally (no reply written, but the command is claimed).
func TestImportSameProjectEmptyFromStillWorks(t *testing.T) {
	endpointRoot := t.TempDir()
	if err := fsq.EnsureRootDirs(endpointRoot); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(endpointRoot, DefaultHandle); err != nil {
		t.Fatal(err)
	}
	store, err := requests.Open(filepath.Join(endpointRoot, "extensions", "remote"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	var carrier *Carrier
	ep := core.New(core.Config{Store: store, Publish: func(s protocol.Snapshot, origin map[string]string) error {
		return carrier.Publish(s, origin)
	}})
	carrier, err = New(endpointRoot, DefaultHandle, ep)
	if err != nil {
		t.Fatalf("carrier: %v", err)
	}
	// No router — same-project only (D2: empty reply_project never touches it).
	rt := fake.New("fake", "e_1")
	ep.Register(rt)
	t.Cleanup(func() { _ = ep.Close() })

	// Command with from:"" and no reply_project — same-project, unusable From.
	body := `{"schema":"amq.remote.command/1","op":"request.submit","request_id":"11111111-1111-4111-8111-111111111341","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(time.Now().Add(time.Minute)) + `","input":{"text":"hi"}}`
	now := time.Now()
	id, _ := format.NewMessageID(now)
	msg := format.Message{Header: format.Header{
		Schema: format.CurrentSchema, ID: id, From: "", To: []string{DefaultHandle},
		Thread: "p2p/codex__remote", Subject: "submit", Created: now.UTC().Format(time.RFC3339Nano), Kind: "todo",
	}, Body: body}
	data, _ := msg.Marshal()
	identity, _ := fsq.SnapshotDeliveryRoot(endpointRoot)
	droot, _ := fsq.OpenDeliveryRoot(endpointRoot, identity)
	if _, err := fsq.DeliverToInboxes(droot, []string{DefaultHandle}, id+".md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	_ = droot.Close()

	n, err := carrier.ImportOnce()
	if err != nil {
		t.Fatalf("same-project from:\"\" returned an error (Pro B2 regression): %v", err)
	}
	if n != 1 {
		t.Fatalf("same-project from:\"\" not handled: n=%d", n)
	}
	// The command was claimed into cur (not left in new).
	if entries, _ := os.ReadDir(fsq.AgentInboxNew(endpointRoot, DefaultHandle)); len(entries) != 0 {
		t.Fatalf("command left in new: %d", len(entries))
	}
	if entries, _ := os.ReadDir(fsq.AgentInboxCur(endpointRoot, DefaultHandle)); len(entries) != 1 {
		t.Fatalf("command not claimed into cur: %d", len(entries))
	}
	// F2: the REAL assertion — published_revision must CONVERGE. With the B2
	// regression (reply to "" fails, publish error swallowed), the record
	// churns every tick and published_revision stays 0.
	k := requests.Key{CreatorHost: "amq:", TargetID: "fake", RequestID: "11111111-1111-4111-8111-111111111341"}
	// Complete the run so a terminal revision is published.
	rt.Complete("11111111-1111-4111-8111-111111111341", "done")
	if err := ep.Tick(); err != nil { // Reconcile republishes
		t.Fatalf("tick: %v", err)
	}
	rec, _, _ := store.Get(k)
	if rec == nil {
		t.Fatal("record not found")
	}
	if rec.PublishedRevision < rec.Revision {
		t.Fatalf("published_revision=%d < revision=%d (F1: same-project from:\"\" publish never converges — reply to empty handle fails every tick)", rec.PublishedRevision, rec.Revision)
	}
}

// TestImportCrossProjectNoPeerMailboxDoesNotCreateIt reproduces Pro B4: the
// probe never checked the destination mailbox exists, and DeliverToInboxes
// creates it — recreating the original black hole inside the PEER root. D4:
// before delivering to a peer root, call ValidateExistingMailboxLayout. A peer
// root whose mailbox does not exist is unroutable — D3 applies (DLQ).
func TestImportCrossProjectNoPeerMailboxDoesNotCreateIt(t *testing.T) {
	endpointRoot := t.TempDir()
	if err := fsq.EnsureRootDirs(endpointRoot); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(endpointRoot, DefaultHandle); err != nil {
		t.Fatal(err)
	}
	store, err := requests.Open(filepath.Join(endpointRoot, "extensions", "remote"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	var carrier *Carrier
	ep := core.New(core.Config{Store: store, Publish: func(s protocol.Snapshot, origin map[string]string) error {
		return carrier.Publish(s, origin)
	}})
	carrier, err = New(endpointRoot, DefaultHandle, ep)
	if err != nil {
		t.Fatalf("carrier: %v", err)
	}
	// Peer root exists but has NO codex mailbox.
	peerRoot := t.TempDir()
	if err := fsq.EnsureRootDirs(peerRoot); err != nil {
		t.Fatal(err)
	}
	// Deliberately do NOT call EnsureAgentDirs(peerRoot, "codex").
	carrier.SetReplyRouter(func(project, replyTo string) (string, string, error) {
		return peerRoot, "codex", nil
	})
	ep.Register(fake.New("fake", "e_1"))
	t.Cleanup(func() { _ = ep.Close() })

	body := `{"schema":"amq.remote.command/1","op":"request.submit","request_id":"11111111-1111-4111-8111-111111111351","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(time.Now().Add(time.Minute)) + `","input":{"text":"hi"}}`
	now := time.Now()
	id, _ := format.NewMessageID(now)
	msg := format.Message{Header: format.Header{
		Schema: format.CurrentSchema, ID: id, From: "codex", To: []string{DefaultHandle},
		Thread: "p2p/codex__remote", Subject: "submit", Created: now.UTC().Format(time.RFC3339Nano), Kind: "todo",
		FromProject: "peer", ReplyTo: "codex@session1", ReplyProject: "peer",
	}, Body: body}
	data, _ := msg.Marshal()
	identity, _ := fsq.SnapshotDeliveryRoot(endpointRoot)
	droot, _ := fsq.OpenDeliveryRoot(endpointRoot, identity)
	if _, err := fsq.DeliverToInboxes(droot, []string{DefaultHandle}, id+".md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	_ = droot.Close()

	// F5: a missing peer mailbox is TRANSIENT (the mailbox may be provisioned
	// a moment later), not poison. The message stays in new for the next tick.
	// It must NOT be DLQ'd, and it must NOT create the peer mailbox.
	n, _ := carrier.ImportOnce()
	_ = n // the message is left in new (transient), so n=0 is expected
	// The peer root must NOT have a codex mailbox created by us.
	peerCodexDir := filepath.Join(peerRoot, "agents", "codex")
	if _, err := os.Stat(peerCodexDir); err == nil {
		t.Fatal("peer root codex mailbox was created (Pro B4 — black hole moved into peer root)")
	}
	// The message stays in new (transient — not DLQ'd).
	if entries, _ := os.ReadDir(fsq.AgentInboxNew(endpointRoot, DefaultHandle)); len(entries) != 1 {
		t.Fatalf("transient (no peer mailbox) message should stay in new: %d entries", len(entries))
	}
	// No DLQ entry.
	dlqDir := filepath.Join(endpointRoot, "agents", DefaultHandle, "dlq", "new")
	if entries, _ := os.ReadDir(dlqDir); len(entries) != 0 {
		t.Fatalf("transient message was DLQ'd (F5: missing peer mailbox is not poison): %d", len(entries))
	}
}
