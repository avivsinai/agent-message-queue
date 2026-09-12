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
	rec, ok, err := store.Get(requests.Key{CreatorHost: SourceHost(format.Header{From: "codex"}), TargetID: "fake", RequestID: "11111111-1111-4111-8111-111111111301"})
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
	hostDir := filepath.Join(stateDir, "v1", "requests", SourceHost(format.Header{From: "codex"}))
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
	ref := protocol.EncodeRef(SourceHost(format.Header{From: "codex"}), "fake", id)
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

// TestSourceHostIsInjective reproduces agent-message-queue-611.22.8 (B03
// identities, host-collapse half): the creator host is part of the request
// KEY, but it was derived by sanitizing a readable string — every illegal
// rune mapped to '-', including the '@' that joined handle and project. So
// From="a" with project "b" and a bare From="a-b" both produced "amq:a-b",
// and unvalidated project names collapsed the same way ("web app" vs
// "web-app"). Two distinct callers then shared ONE record: the second submit
// was answered with the first's snapshot, and a cancel from one terminated
// the other's run.
func TestSourceHostIsInjective(t *testing.T) {
	cases := []format.Header{
		{From: "a", FromProject: "b"},
		{From: "a-b"},
		{From: "a", FromProject: "b-c"},
		{From: "a-b", FromProject: "c"},
		{From: "codex", FromProject: "web app"},
		{From: "codex", FromProject: "web-app"},
		{From: "codex"},
		{From: "codex", FromProject: ""},
	}
	seen := map[string][]format.Header{}
	for _, h := range cases {
		got := SourceHost(h)
		seen[got] = append(seen[got], h)
	}
	for host, headers := range seen {
		// {From:"codex"} and {From:"codex", FromProject:""} are the SAME
		// identity, so they may share a host; anything else may not.
		distinct := map[[2]string]bool{}
		for _, h := range headers {
			distinct[[2]string{h.From, h.FromProject}] = true
		}
		if len(distinct) > 1 {
			t.Fatalf("host %q is shared by %d distinct identities: %v", host, len(distinct), headers)
		}
	}
	// The readable prefix must survive for humans and logs.
	if got := SourceHost(format.Header{From: "codex", FromProject: "amq"}); !strings.HasPrefix(got, "amq:codex.amq.") {
		t.Fatalf("host %q lost its readable prefix", got)
	}
	// And the derived host must still be usable as a path segment.
	for _, h := range cases {
		got := SourceHost(h)
		if strings.ContainsAny(got, "/\\ ") {
			t.Fatalf("host %q is not path-safe", got)
		}
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
