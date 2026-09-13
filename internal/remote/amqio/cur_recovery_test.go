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
	rec, ok, err := store.Get(requests.Key{CreatorHost: "amq:codex", TargetID: "fake", RequestID: id})
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
		Host:   "amq:" + from,
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
