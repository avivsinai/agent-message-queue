package amqio

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// newCarrierEnv wires a store, endpoint, fake target and carrier over one
// AMQ root — the shared scaffold of the cur-recovery tests.
func newCarrierEnv(t *testing.T) (root string, carrier *Carrier, rt *fake.Runtime, store *requests.Store) {
	t.Helper()
	root = t.TempDir()
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
	var c *Carrier
	ep := core.New(core.Config{Store: store, Publish: func(s protocol.Snapshot, o map[string]string) error {
		return c.Publish(s, o)
	}})
	c, err = New(root, DefaultHandle, ep)
	if err != nil {
		t.Fatalf("carrier: %v", err)
	}
	rt = fake.New("fake", "e_1")
	ep.Register(rt)
	t.Cleanup(func() { _ = ep.Close() })
	return root, c, rt, store
}

// simulateCrashBetweenClaimAndReceipt reproduces the B09 crash gap directly:
// the message has already been claimed into cur (MoveNewToCur done), but the
// process died before receipt.EmitDeliveryRoot (and, in the worst ordering,
// before the outcome reply reached the sender). On disk this is exactly: a
// message in inbox/cur, no drained receipt, and no outcome reply to sender.
func simulateCrashBetweenClaimAndReceipt(t *testing.T, root, id, body string) {
	t.Helper()
	now := time.Now()
	mid, err := format.NewMessageID(now)
	if err != nil {
		t.Fatal(err)
	}
	msg := format.Message{Header: format.Header{
		Schema: format.CurrentSchema, ID: mid, From: "codex", To: []string{DefaultHandle},
		Thread: "p2p/codex__remote", Subject: "submit", Created: now.UTC().Format(time.RFC3339Nano), Kind: "todo",
	}, Body: body}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	curPath := filepath.Join(fsq.AgentInboxCur(root, DefaultHandle), mid+".md")
	if err := os.MkdirAll(fsq.AgentInboxCur(root, DefaultHandle), 0o700); err != nil {
		t.Fatal(err)
	}
	// fsq.DeliverToInboxes writes through the delivery root; for a cur-path
	// fixture a plain atomic write matches what MoveNewToCur left behind.
	tmp := curPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, curPath); err != nil {
		t.Fatal(err)
	}
	_ = id
}

// TestCurRecoveryEmitsMissingReceiptAndOutcome pins the B09 cur-recovery
// scan: a command claimed into cur that crashed between MoveNewToCur and the
// receipt (and possibly before the outcome reply) is reconciled on the next
// ImportOnce — the missing drained receipt is emitted, the sender receives a
// reply reconstructed from the DURABLE RECORD (the command is never
// re-executed: native dispatches stay where the record left them), and the
// message stays in cur (recovery is bookkeeping, not a re-drain).
func TestCurRecoveryEmitsMissingReceiptAndOutcome(t *testing.T) {
	root, carrier, rt, store := newCarrierEnv(t)

	// The endpoint already handled this submit — the record is durable —
	// but the process crashed after Handle returned and before the carrier
	// could emit the receipt (and the reply the sender waits for).
	id := "11111111-1111-4111-8111-1111111111f1"
	cmd := &protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit,
		RequestID: id, TargetID: "fake", Epoch: "e_1",
		NotAfter: protocol.FormatTime(time.Now().Add(time.Minute)),
		Input:    &protocol.SubmitInput{Text: "recovered"},
	}
	if _, err := carrier.ep.Handle(cmd, coreSrc("codex")); err != nil {
		t.Fatalf("pre-crash handle: %v", err)
	}

	// Now stage the crash gap: the message claimed into cur, receipt lost.
	body := `{"schema":"amq.remote.command/1","op":"request.submit","request_id":"` + id + `","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(time.Now().Add(time.Minute)) + `","input":{"text":"recovered"}}`
	simulateCrashBetweenClaimAndReceipt(t, root, id, body)

	before := rt.Snapshot().Dispatches

	n, err := carrier.ImportOnce()
	if err != nil || n != 0 {
		t.Fatalf("import over cur-only fixture: n=%d err=%v", n, err)
	}

	// 1. The missing drained receipt now exists.
	if _, err := receipt.WaitFor(root, func() string {
		entries, _ := os.ReadDir(fsq.AgentInboxCur(root, DefaultHandle))
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".md") {
				return strings.TrimSuffix(e.Name(), ".md")
			}
		}
		return ""
	}(), DefaultHandle, receipt.StageDrained, time.Second, 5*time.Millisecond); err != nil {
		t.Fatalf("no recovered drained receipt: %v", err)
	}

	// 2. The sender learned the outcome, reconstructed from the record.
	entries, err := os.ReadDir(fsq.AgentInboxNew(root, "codex"))
	if err != nil {
		t.Fatal(err)
	}
	gotReply := false
	for _, e := range entries {
		m, err := format.ReadMessageFile(filepath.Join(fsq.AgentInboxNew(root, "codex"), e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if m.Header.Thread == "p2p/codex__remote" {
			gotReply = true
		}
	}
	if !gotReply {
		t.Fatal("no outcome reply reconstructed for the sender")
	}

	// 3. The command was NOT re-executed: no second native dispatch, and the
	// record is exactly where the pre-crash endpoint left it.
	if got := rt.Snapshot().Dispatches; got != before {
		t.Fatalf("recovery re-executed the command: dispatches %d -> %d", before, got)
	}
	rec, ok, err := store.Get(requests.Key{CreatorHost: SourceHost(format.Header{From: "codex"}), TargetID: "fake", RequestID: id})
	if err != nil || !ok {
		t.Fatalf("record: ok=%v err=%v", ok, err)
	}
	if rec.State != protocol.StateReceived && rec.State != protocol.StateRunning {
		t.Fatalf("unexpected record state after recovery: %s", rec.State)
	}

	// 4. The message stays in cur — recovery does not re-drain claimed mail.
	if entries, _ := os.ReadDir(fsq.AgentInboxCur(root, DefaultHandle)); len(entries) != 1 {
		t.Fatalf("recovery moved the cur message: %d entries", len(entries))
	}

	// 5. Second recovery is idempotent: no duplicate replies, no error.
	if _, err := carrier.ImportOnce(); err != nil {
		t.Fatalf("second recovery: %v", err)
	}
	entries, _ = os.ReadDir(fsq.AgentInboxNew(root, "codex"))
	if len(entries) != 1 {
		t.Fatalf("duplicate outcome reply on second recovery: %d", len(entries))
	}
}

// coreSrc builds the Source the real importOne path passes to ep.Handle:
// a Host derived from the message header, and an Origin map carrying the
// carrier routing metadata ("carrier": "amq") so Publish actually delivers.
// The old coreSrc omitted Origin, which made Publish return early and
// suppressed the very message the real path sends — hiding double-delivery.
func coreSrc(from string) core.Source {
	return core.Source{
		Host:   SourceHost(format.Header{From: from}),
		Origin: map[string]string{"carrier": "amq", "from": from, "thread": "p2p/" + from + "__remote"},
	}
}

// deliverOneSubmit seeds one submit command into inbox/new and returns its
// message id + request id.
func deliverOneSubmit(t *testing.T, root, reqID string) string {
	t.Helper()
	body := `{"schema":"amq.remote.command/1","op":"request.submit","request_id":"` + reqID + `","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(time.Now().Add(time.Minute)) + `","input":{"text":"hi"}}`
	now := time.Now()
	mid, err := format.NewMessageID(now)
	if err != nil {
		t.Fatal(err)
	}
	msg := format.Message{Header: format.Header{Schema: format.CurrentSchema, ID: mid, From: "codex", To: []string{DefaultHandle}, Thread: "p2p/codex__remote", Subject: "submit", Created: now.UTC().Format(time.RFC3339Nano), Kind: "todo"}, Body: body}
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
	if _, err := fsq.DeliverToInboxes(droot, []string{DefaultHandle}, mid+".md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	_ = droot.Close()
	return mid
}

// TestCurRecoverySteadyStateDoesNotRescan asserts that the full cur sweep
// runs ONCE (on the first ImportOnce) and steady-state ImportOnce calls do
// NOT re-read every cur entry. The seam is curSweepCount, incremented only by
// recoverCur (the full sweep). Steady-state reconciliation goes through
// recoverClaimed, which stats receipts by message ID — no ReadDir of cur.
func TestCurRecoverySteadyStateDoesNotRescan(t *testing.T) {
	root, carrier, _, _ := newCarrierEnv(t)

	// Claim 5 commands into cur via ImportOnce (the first call also does the
	// full recovery sweep — cur is empty, so it's a no-op sweep).
	for i := 0; i < 5; i++ {
		deliverOneSubmit(t, root, fmt.Sprintf("11111111-1111-4111-8111-1111111111%02d", i+1))
	}
	n, err := carrier.ImportOnce()
	if err != nil || n != 5 {
		t.Fatalf("first import: n=%d err=%v (want 5)", n, err)
	}
	sweepsAfterFirst := carrier.curSweepCount
	if sweepsAfterFirst != 1 {
		t.Fatalf("first import: curSweepCount=%d (want 1 — full sweep on first ImportOnce)", sweepsAfterFirst)
	}
	// All 5 are now in cur with receipts.
	if entries, _ := os.ReadDir(fsq.AgentInboxCur(root, DefaultHandle)); len(entries) != 5 {
		t.Fatalf("cur entries: %d (want 5)", len(entries))
	}

	// Second ImportOnce: no new messages, all receipts present. The steady-
	// state path (recoverClaimed) stats each receipt and drops the entries.
	// It must NOT call recoverCur (no full ReadDir of cur).
	n, err = carrier.ImportOnce()
	if err != nil || n != 0 {
		t.Fatalf("second import: n=%d err=%v (want 0 — no new messages)", n, err)
	}
	sweepsAfterSecond := carrier.curSweepCount
	if sweepsAfterSecond != 1 {
		t.Fatalf("second import: curSweepCount=%d (want 1 — steady state must NOT re-scan cur; Pro #752 blocker)", sweepsAfterSecond)
	}
}

// TestCurRecoveryCorruptedReceiptIsRecovered pins the ruling: a receipt that
// EXISTS BUT DOES NOT PARSE is not proof the case was closed — the sweep must
// recover it (rewrite the receipt, send the reply), not skip it. An
// unparseable receipt cannot tell us anything, and presence is not proof.
func TestCurRecoveryCorruptedReceiptIsRecovered(t *testing.T) {
	root, carrier, rt, store := newCarrierEnv(t)

	id := "11111111-1111-4111-8111-1111111111c1"
	cmd := &protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit,
		RequestID: id, TargetID: "fake", Epoch: "e_1",
		NotAfter: protocol.FormatTime(time.Now().Add(time.Minute)),
		Input:    &protocol.SubmitInput{Text: "corrupted-receipt-test"},
	}
	if _, err := carrier.ep.Handle(cmd, coreSrc("codex")); err != nil {
		t.Fatalf("pre-crash handle: %v", err)
	}

	body := `{"schema":"amq.remote.command/1","op":"request.submit","request_id":"` + id + `","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(time.Now().Add(time.Minute)) + `","input":{"text":"corrupted-receipt-test"}}`
	simulateCrashBetweenClaimAndReceipt(t, root, id, body)

	// Corrupt the receipt: write garbage at the receipt path so it EXISTS but
	// does NOT parse. The sweep must treat this like a missing receipt.
	curEntries, _ := os.ReadDir(fsq.AgentInboxCur(root, DefaultHandle))
	var msgID string
	for _, e := range curEntries {
		if strings.HasSuffix(e.Name(), ".md") {
			msgID = strings.TrimSuffix(e.Name(), ".md")
			break
		}
	}
	if msgID == "" {
		t.Fatal("no cur entry found")
	}
	receiptDir := filepath.Join(root, "agents", DefaultHandle, "receipts")
	if err := os.MkdirAll(receiptDir, 0o700); err != nil {
		t.Fatal(err)
	}
	receiptPath := filepath.Join(receiptDir, msgID+"__"+DefaultHandle+"__"+receipt.StageDrained+".json")
	if err := os.WriteFile(receiptPath, []byte("{CORRUPTED"), 0o600); err != nil {
		t.Fatal(err)
	}

	before := rt.Snapshot().Dispatches

	n, err := carrier.ImportOnce()
	if err != nil || n != 0 {
		t.Fatalf("import over corrupted-receipt fixture: n=%d err=%v", n, err)
	}

	// 1. The receipt was rewritten and now PARSES.
	rc, err := receipt.ReadDeliveryRoot(
		mustOpenDeliveryRoot(t, root),
		filepath.Join("agents", DefaultHandle, "receipts", msgID+"__"+DefaultHandle+"__"+receipt.StageDrained+".json"),
	)
	if err != nil {
		t.Fatalf("corrupted receipt was not repaired: %v", err)
	}
	if rc.MsgID != msgID {
		t.Fatalf("repaired receipt has wrong MsgID: got %s want %s", rc.MsgID, msgID)
	}

	// 2. The sender received the reply.
	entries, _ := os.ReadDir(fsq.AgentInboxNew(root, "codex"))
	gotReply := false
	for _, e := range entries {
		m, err := format.ReadMessageFile(filepath.Join(fsq.AgentInboxNew(root, "codex"), e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if m.Header.Thread == "p2p/codex__remote" {
			gotReply = true
		}
	}
	if !gotReply {
		t.Fatal("no outcome reply for corrupted-receipt recovery")
	}

	// 3. The command was NOT re-executed.
	if got := rt.Snapshot().Dispatches; got != before {
		t.Fatalf("recovery re-executed: %d -> %d", before, got)
	}
	_ = store
}

// TestCurRecoveryIdempotentReply proves the recovery reply is idempotent by
// construction: recovering the same command twice produces exactly ONE reply
// file in the caller's inbox, because the deterministic message id makes the
// second write a byte-identical collision (resolvePublishCollision -> nil).
//
// This test does NOT pre-handle the command, so there is no durable record and
// no PublishedRevision — the recovery reply IS sent (reconstructReply returns
// a Refuse(CodeNotFound), which is a valid reply). This isolates the recovery
// reply path from the Publish path.
func TestCurRecoveryIdempotentReply(t *testing.T) {
	root, carrier, _, _ := newCarrierEnv(t)

	id := "11111111-1111-4111-8111-1111111111a1"
	body := `{"schema":"amq.remote.command/1","op":"request.submit","request_id":"` + id + `","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(time.Now().Add(time.Minute)) + `","input":{"text":"idempotent-test"}}`
	simulateCrashBetweenClaimAndReceipt(t, root, id, body)

	// First recovery.
	if _, err := carrier.ImportOnce(); err != nil {
		t.Fatalf("first recovery: %v", err)
	}
	entries1, _ := os.ReadDir(fsq.AgentInboxNew(root, "codex"))
	if len(entries1) != 1 {
		t.Fatalf("expected 1 reply after first recovery, got %d", len(entries1))
	}

	// Second recovery — must NOT add a second reply file.
	if _, err := carrier.ImportOnce(); err != nil {
		t.Fatalf("second recovery: %v", err)
	}
	entries2, _ := os.ReadDir(fsq.AgentInboxNew(root, "codex"))
	if len(entries2) != 1 {
		t.Fatalf("idempotent recovery failed: expected 1 reply, got %d (duplicate written)", len(entries2))
	}

	// The filename must be identical (same deterministic id).
	if entries1[0].Name() != entries2[0].Name() {
		t.Fatalf("reply filename changed between recoveries: %s -> %s", entries1[0].Name(), entries2[0].Name())
	}

	// The recovery reply's filename must contain _recover_ (not _publish_rev).
	if !strings.Contains(entries1[0].Name(), "_recover_") {
		t.Fatalf("reply filename is not a recovery reply: %s", entries1[0].Name())
	}
}

// TestCurRecoveryReplyTimestampPrefix asserts the recovery reply id has the
// timestamp-first shape (same ordering guarantee as B10's Publish id) so
// drain --limit 20 does not starve recovery replies.
//
// No pre-handle: isolates the recovery reply path from Publish.
func TestCurRecoveryReplyTimestampPrefix(t *testing.T) {
	root, carrier, _, _ := newCarrierEnv(t)

	id := "11111111-1111-4111-8111-1111111111b1"
	body := `{"schema":"amq.remote.command/1","op":"request.submit","request_id":"` + id + `","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(time.Now().Add(time.Minute)) + `","input":{"text":"timestamp-test"}}`
	simulateCrashBetweenClaimAndReceipt(t, root, id, body)

	if _, err := carrier.ImportOnce(); err != nil {
		t.Fatalf("recovery: %v", err)
	}

	entries, _ := os.ReadDir(fsq.AgentInboxNew(root, "codex"))
	if len(entries) != 1 {
		t.Fatalf("expected 1 reply, got %d", len(entries))
	}
	name := entries[0].Name()
	// Must start with a RFC3339-like timestamp (YYYY-MM-DD...), not "recover_".
	if !strings.HasPrefix(name, "20") {
		t.Fatalf("recovery reply filename does not start with a timestamp: %s", name)
	}
	if !strings.Contains(name, "_recover_") {
		t.Fatalf("recovery reply filename missing _recover_ marker: %s", name)
	}
}

// mustOpenDeliveryRoot is a test helper that opens a delivery root or fails.
func mustOpenDeliveryRoot(t *testing.T, root string) *fsq.DeliveryRoot {
	t.Helper()
	identity, err := fsq.SnapshotDeliveryRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	dr, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dr.Close() })
	return dr
}

// TestCurRecoveryLostReplyIsResent proves the reply is not gated on
// !hasReceipt. Scenario: the receipt was emitted but the reply was lost
// (crash between receipt and reply). On the next sweep, the receipt EXISTS
// and PARSES, but the reply is STILL sent — because it is idempotent, a
// duplicate is free. A third sweep adds nothing.
//
// Mutation: re-gate the reply inside !hasReceipt -> this test fails because
// the caller never receives the reply (hasReceipt=true -> skip).
func TestCurRecoveryLostReplyIsResent(t *testing.T) {
	root, carrier, _, store := newCarrierEnv(t)

	id := "11111111-1111-4111-8111-1111111111e1"
	body := `{"schema":"amq.remote.command/1","op":"request.submit","request_id":"` + id + `","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(time.Now().Add(time.Minute)) + `","input":{"text":"lost-reply-test"}}`
	simulateCrashBetweenClaimAndReceipt(t, root, id, body)

	// First recovery: emits the receipt AND the reply.
	if _, err := carrier.ImportOnce(); err != nil {
		t.Fatalf("first recovery: %v", err)
	}
	entries1, _ := os.ReadDir(fsq.AgentInboxNew(root, "codex"))
	if len(entries1) != 1 {
		t.Fatalf("expected 1 reply after first recovery, got %d", len(entries1))
	}

	// Simulate the crash gap: the receipt landed, but the reply did not.
	// Delete the reply file (the caller's inbox lost it).
	if err := os.Remove(filepath.Join(fsq.AgentInboxNew(root, "codex"), entries1[0].Name())); err != nil {
		t.Fatal(err)
	}

	// Second recovery: the receipt EXISTS and PARSES (hasReceipt=true), but
	// the reply must STILL be sent because it is idempotent and ungated.
	// Simulate a process restart: new carrier over the same root + store,
	// so the full cur sweep runs again.
	carrier2, _ := newCarrierOverExisting(t, root, store)
	if _, err := carrier2.ImportOnce(); err != nil {
		t.Fatalf("second recovery: %v", err)
	}
	entries2, _ := os.ReadDir(fsq.AgentInboxNew(root, "codex"))
	if len(entries2) != 1 {
		t.Fatalf("lost reply was not resent: expected 1 reply, got %d", len(entries2))
	}

	// The filename must be identical (same deterministic recovery id).
	if entries1[0].Name() != entries2[0].Name() {
		t.Fatalf("resent reply has different filename: %s -> %s", entries1[0].Name(), entries2[0].Name())
	}

	// Third sweep: adds nothing (idempotent collision).
	if _, err := carrier2.ImportOnce(); err != nil {
		t.Fatalf("third recovery: %v", err)
	}
	entries3, _ := os.ReadDir(fsq.AgentInboxNew(root, "codex"))
	if len(entries3) != 1 {
		t.Fatalf("third recovery added a duplicate: expected 1, got %d", len(entries3))
	}
}

// newCarrierOverExisting creates a fresh Carrier over an existing root and
// store, simulating a process restart so the full cur sweep runs again.
func newCarrierOverExisting(t *testing.T, root string, store *requests.Store) (*Carrier, *fake.Runtime) {
	t.Helper()
	var c *Carrier
	ep := core.New(core.Config{Store: store, Publish: func(s protocol.Snapshot, o map[string]string) error {
		return c.Publish(s, o)
	}})
	c, err := New(root, DefaultHandle, ep)
	if err != nil {
		t.Fatalf("carrier: %v", err)
	}
	rt := fake.New("fake", "e_1")
	ep.Register(rt)
	t.Cleanup(func() { _ = ep.Close() })
	return c, rt
}
