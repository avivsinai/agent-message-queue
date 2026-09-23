package buzzio

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// codex #866 r1 #4: the same signed DM, busy-rejected once, was dispatched
// when it was redelivered after the session became idle.
func TestBusyRejectedDMRedeliveryDoesNotDispatch(t *testing.T) {
	store, err := requests.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ep := core.New(core.Config{Store: store, Now: func() time.Time { return now }})
	t.Cleanup(func() { _ = ep.Close() })
	rt := fake.New("cx", "e1")
	ep.Register(rt)
	firstID := "11111111-1111-4111-8111-111111111111"
	first := &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit, RequestID: firstID, TargetID: "cx", Epoch: "e1", NotAfter: protocol.FormatTime(now.Add(time.Minute)), Input: &protocol.SubmitInput{Text: "occupy the session", Busy: protocol.BusyReject, Deliver: protocol.DeliverTurn}}
	if _, err := ep.Handle(first, core.Source{Host: "local"}); err != nil {
		t.Fatal(err)
	}
	if !rt.HasRun(firstID) {
		t.Fatal("first request did not occupy fake runtime")
	}
	var owner, body [32]byte
	_, _ = rand.Read(owner[:])
	_, _ = rand.Read(body[:])
	b := Binding{Owner: nostr.GetPublicKey(owner).Hex(), Body: nostr.GetPublicKey(body).Hex(), Channel: "dm-1", Target: "cx", RelayHost: "relay", NativeSession: "cx"}
	ledger, err := OpenLedger(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := NewCarrier(ledger, b, body, ownerGrant(t, owner, b.Body, KindDM, KindEdit), ep.NativeSessionID, ep.Handle)
	c.now = func() time.Time { return now }
	dm := ownerEvent(t, owner, b.Channel, "run this once", now)
	if err := c.Ingest(dm); err != nil {
		t.Fatal(err)
	}
	n, err := Normalize(dm, b, now)
	if err != nil {
		t.Fatal(err)
	}
	key := requests.Key{CreatorHost: c.source(dm.ID.Hex(), "").Host, TargetID: b.Target, RequestID: n.RequestID}
	rec, ok, err := store.Get(key)
	if err != nil || !ok || rec.State != protocol.StateRejected || rec.Code != protocol.CodeBusy {
		t.Fatalf("first delivery: rec=%+v ok=%v err=%v", rec, ok, err)
	}
	before := rt.Snapshot().Dispatches
	if !rt.Complete(firstID, "done") {
		t.Fatal("could not release first request")
	}
	now = now.Add(2 * time.Second)
	if err := c.Ingest(dm); err != nil {
		t.Fatal(err)
	}
	rec, _, err = store.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	after := rt.Snapshot().Dispatches
	if after != before {
		t.Fatalf("same signed DM was busy-rejected, then redelivery after session became idle changed state to %s and native dispatch count from %d to %d", rec.State, before, after)
	}
}

// codex #866 r1 #1: /cancel sent request.cancel without target, epoch and
// deadline, so the real endpoint refused it and no native cancel ran.
func TestDMCancelReachesTheNativeAdapter(t *testing.T) {
	owner, body := nostr.Generate(), nostr.Generate()
	b := Binding{Owner: nostr.GetPublicKey(owner).Hex(), Body: nostr.GetPublicKey(body).Hex(), Channel: "dm-1", Target: "cx", RelayHost: "relay", NativeSession: "cx"}
	ledger, err := OpenLedger(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := requests.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ep := core.New(core.Config{Store: store})
	defer func() { _ = ep.Close() }()
	rt := fake.New("cx", "e_1")
	ep.Register(rt)
	var cancelErr error
	c := NewCarrier(ledger, b, body, ownerGrant(t, owner, b.Body, KindDM, KindEdit), fixedIdentity("cx"), func(cmd *protocol.Command, src core.Source) (any, error) {
		result, err := ep.Handle(cmd, src)
		if cmd.Op == protocol.OpRequestCancel {
			cancelErr = err
		}
		return result, err
	})
	now := time.Now()
	dm := ownerEvent(t, owner, b.Channel, "long job", now)
	if err := c.Ingest(dm); err != nil {
		t.Fatal(err)
	}
	ref := protocol.EncodeRef(c.sourceFor(dm).Host, b.Target, requestIDFor(b, dm.ID.Hex()))
	if _, ok, err := ledger.ReceiptFor(ref); err != nil || !ok {
		t.Fatalf("missing receipt: %v", err)
	}
	if err := c.Ingest(ownerEvent(t, owner, b.Channel, "/cancel "+ref, now)); err != nil {
		t.Fatal(err)
	}
	if rt.CancelExactCount() != 1 {
		t.Fatalf("native cancels=%d, endpoint error=%v", rt.CancelExactCount(), cancelErr)
	}
}

// codex #866 r1 #5: a crash after the root row was prepared but before its
// receipt was saved let the next revision prepare a second kind 9 root. The
// root obligation is revision-independent and is recovered instead.
func TestRootRowRecoveredAfterCrashBeforeReceipt(t *testing.T) {
	var owner, body [32]byte
	_, _ = rand.Read(owner[:])
	_, _ = rand.Read(body[:])
	b := Binding{Owner: nostr.GetPublicKey(owner).Hex(), Body: nostr.GetPublicKey(body).Hex(), Channel: "dm-1", Target: "cx", RelayHost: "relay", NativeSession: "cx"}
	ledger, _ := OpenLedger(t.TempDir())
	c := NewCarrier(ledger, b, body, ownerGrant(t, owner, b.Body, KindDM, KindEdit), fixedIdentity("cx"), nil)
	now := time.Now()
	c.now = func() time.Time { return now }
	if err := ledger.PutReceipt(c.receiptFor("amqr1_x", Claim{Target: "cx", Epoch: "e1"})); err != nil {
		t.Fatal(err)
	}
	// The crash: revision 1's root is in the outbox, the receipt never saw it.
	root := nostr.Event{CreatedAt: nostr.Timestamp(now.Unix()), Kind: KindDM, Tags: c.rowTags("", ""), Content: "running"}
	if _, err := c.prepareRow(rootKey("amqr1_x"), root, 1); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	origin := c.source("", "").Origin
	if err := c.Publish(protocol.Snapshot{RequestRef: "amqr1_x", Revision: 2, State: protocol.StateCompleted}, origin); err != nil {
		t.Fatal(err)
	}
	var sent []nostr.Event
	if err := c.Flush(context.Background(), func(_ context.Context, evt nostr.Event) error { sent = append(sent, evt); return nil }, nil); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 || sent[0].Kind != KindDM || sent[1].Kind != KindEdit || tagValue(sent[1], "e") != sent[0].ID.Hex() {
		t.Fatalf("sent = %+v, want the one recovered root then its edit", sent)
	}
}
