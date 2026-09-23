package buzzio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// Regressions for codex's second review of PR #866; each test names its
// finding. They run the real endpoint with the fake runtime.

type rig struct {
	now         time.Time
	owner, body [32]byte
	b           Binding
	l           *Ledger
	ep          *core.Endpoint
	rt          *fake.Runtime
	store       *requests.Store
	c           *Carrier
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{now: time.Now(), owner: nostr.Generate(), body: nostr.Generate()}
	r.b = Binding{Owner: nostr.GetPublicKey(r.owner).Hex(), Body: nostr.GetPublicKey(r.body).Hex(), Channel: "dm-1", Target: "cx", RelayHost: "relay", NativeSession: "cx"}
	var err error
	if r.l, err = OpenLedger(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if r.store, err = requests.Open(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	r.ep = core.New(core.Config{Store: r.store, Now: func() time.Time { return r.now }})
	t.Cleanup(func() { _ = r.ep.Close() })
	r.rt = fake.New("cx", "e1")
	r.ep.Register(r.rt)
	r.c = r.carrier(t, r.b)
	return r
}

func (r *rig) carrier(t *testing.T, b Binding) *Carrier {
	c := NewCarrier(r.l, b, r.body, ownerGrant(t, r.owner, b.Body, KindDM, KindEdit), r.ep.NativeSessionID, r.ep.Handle)
	c.now = func() time.Time { return r.now }
	return c
}

// interruptAfterSubmit ingests dm and stops the edge right after the real
// endpoint answered the submit, leaving the claim unsettled.
func interruptAfterSubmit(t *testing.T, r *rig, dm nostr.Event) {
	t.Helper()
	stop := errors.New("edge interrupted after the endpoint answered")
	r.c.handle = func(cmd *protocol.Command, src core.Source) (any, error) {
		out, err := r.ep.Handle(cmd, src)
		if cmd.Op == protocol.OpRequestSubmit {
			panic(stop)
		}
		return out, err
	}
	func() {
		defer func() {
			if got := recover(); got != stop {
				t.Fatalf("interruption = %v", got)
			}
		}()
		_ = r.c.Ingest(dm)
	}()
	r.c.handle = r.ep.Handle
}

// r2 #1: an interruption between a busy rejection and its settlement
// re-executed the same signed DM once the session became idle.
func TestInterruptedBusyRejectionIsNotReExecuted(t *testing.T) {
	r := newRig(t)
	firstID := "11111111-1111-4111-8111-111111111111"
	first := &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit, RequestID: firstID, TargetID: "cx", Epoch: "e1", NotAfter: protocol.FormatTime(r.now.Add(time.Minute)), Input: &protocol.SubmitInput{Text: "occupy", Busy: protocol.BusyReject, Deliver: protocol.DeliverTurn}}
	if _, err := r.ep.Handle(first, core.Source{Host: "local"}); err != nil {
		t.Fatal(err)
	}
	dm := ownerEvent(t, r.owner, r.b.Channel, "run once", r.now)
	interruptAfterSubmit(t, r, dm)
	before := r.rt.Snapshot().Dispatches
	if !r.rt.Complete(firstID, "done") {
		t.Fatal("could not release the runtime")
	}
	r.now = r.now.Add(2 * time.Second)
	if err := r.c.Ingest(dm); err != nil {
		t.Fatal(err)
	}
	if after := r.rt.Snapshot().Dispatches; after != before {
		t.Fatalf("interrupted busy rejection re-executed: dispatches %d -> %d", before, after)
	}
}

// r2 #2: an interruption after admission but before the receipt stranded
// the result: reconciliation had no receipt to publish under.
func TestInterruptedAdmissionStillPublishesItsRow(t *testing.T) {
	r := newRig(t)
	r.ep.SetPublish(r.c.Publish)
	dm := ownerEvent(t, r.owner, r.b.Channel, "run once", r.now)
	interruptAfterSubmit(t, r, dm)
	r.now = r.now.Add(3 * time.Minute)
	if err := r.ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if pending, err := r.l.Pending(); err != nil || len(pending) == 0 {
		t.Fatalf("admitted request has no result row after reconcile: pending=%d err=%v", len(pending), err)
	}
}

// r2 #3: a settlement recorded before a failed answer left /inspect settled
// and never answered.
func TestFailedAnswerIsRecoveredOnRedelivery(t *testing.T) {
	r := newRig(t)
	grant := r.c.grant
	calls := 0
	r.c.grant = func(kind uint16, at time.Time) (bodykey.AuthTag, error) {
		calls++
		if calls == 3 { // after the two eligibility reads: the answer's signing
			return bodykey.AuthTag{}, errors.New("enrollment briefly unreadable")
		}
		return grant(kind, at)
	}
	dm := ownerEvent(t, r.owner, r.b.Channel, "/inspect", r.now)
	if err := r.c.Ingest(dm); !errors.Is(err, ErrNoGrant) {
		t.Fatalf("first delivery: err=%v, want the signing failure", err)
	}
	r.c.grant = grant
	if err := r.c.Ingest(dm); err != nil {
		t.Fatal(err)
	}
	if pending, _ := r.l.Pending(); len(pending) != 1 {
		t.Fatalf("owed answers = %d, want the recovered answer", len(pending))
	}
}

// r2 #4: output prepared under one relay was sent after the share moved to
// another relay with the same body and channel.
func TestOutputIsNotSentUnderAnotherRelay(t *testing.T) {
	r := newRig(t)
	if err := r.c.Ingest(ownerEvent(t, r.owner, r.b.Channel, "/inspect", r.now)); err != nil {
		t.Fatal(err)
	}
	moved := r.b
	moved.RelayHost = "another-relay"
	sent := 0
	if err := r.carrier(t, moved).Flush(context.Background(), func(context.Context, nostr.Event) error { sent++; return nil }, nil); err != nil {
		t.Fatal(err)
	}
	if sent != 0 {
		t.Fatalf("sent %d output(s) prepared under another relay", sent)
	}
}

// r2 #5: an edit prepared just before a crash lost its receipt update, and
// the next revision was dated in the same second, so Buzz could not order
// them. The edit's second is reserved before the edit is prepared.
func TestEditAfterCrashIsDatedStrictlyLater(t *testing.T) {
	r := newRig(t)
	ref := "amqr1_test"
	if err := r.l.PutReceipt(r.c.receiptFor(ref, Claim{Target: "cx", Epoch: "e1"})); err != nil {
		t.Fatal(err)
	}
	origin := r.c.source("", "").Origin
	if err := r.c.Publish(protocol.Snapshot{RequestRef: ref, Revision: 1, State: protocol.StateRunning}, origin); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(time.Second)
	// The crash: revision 2's edit is prepared, its receipt update is lost.
	crash := errors.New("crash after the edit was prepared")
	editPrepared = func() { panic(crash) }
	func() {
		defer func() { editPrepared = func() {}; _ = recover() }()
		_ = r.c.Publish(protocol.Snapshot{RequestRef: ref, Revision: 2, State: protocol.StateRunning}, origin)
	}()
	if err := r.c.Publish(protocol.Snapshot{RequestRef: ref, Revision: 3, State: protocol.StateCompleted}, origin); !errors.Is(err, ErrClockBehind) {
		t.Fatalf("same-second revision: err=%v, want ErrClockBehind", err)
	}
	r.now = r.now.Add(time.Second)
	if err := r.c.Publish(protocol.Snapshot{RequestRef: ref, Revision: 3, State: protocol.StateCompleted}, origin); err != nil {
		t.Fatal(err)
	}
	var edits []nostr.Event
	_ = r.c.Flush(context.Background(), func(_ context.Context, evt nostr.Event) error {
		if evt.Kind == KindEdit {
			edits = append(edits, evt)
		}
		return nil
	}, nil)
	if len(edits) != 2 || edits[1].CreatedAt <= edits[0].CreatedAt {
		t.Fatalf("edits = %d, want two strictly later edits", len(edits))
	}
}

// r2 #6: native_session_id was added to amq.remote.session/1, whose frozen
// schema forbids additional properties.
func TestSessionProjectionStaysOnTheV1Schema(t *testing.T) {
	schema, err := jsonschema.NewCompiler().Compile("../../../schemas/remote-session-v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(fake.New("cx", "e1").Inspect())
	var doc any
	_ = json.Unmarshal(raw, &doc)
	if err := schema.Validate(doc); err != nil {
		t.Fatalf("session projection breaks the v1 schema: %v", err)
	}
}

// r2 #7: /inspect answered for a replacement native session attached after
// the surface was verified.
func TestInspectRefusesAReplacementNativeSession(t *testing.T) {
	r := newRig(t)
	native := "cx"
	r.c.identity = func(string) string { return native }
	if err := r.c.Shared(); err != nil {
		t.Fatal(err)
	}
	native = "replacement"
	if err := r.c.Ingest(ownerEvent(t, r.owner, r.b.Channel, "/inspect", r.now)); err != nil {
		t.Fatal(err)
	}
	pending, _ := r.l.Pending()
	for _, p := range pending {
		var evt nostr.Event
		_ = json.Unmarshal(p.Event, &evt)
		if evt.Content != ErrNotShared.Error() {
			t.Fatalf("answered a replacement session: %q", evt.Content)
		}
	}
}

// r2 #8: sixteen owed answers from an earlier channel filled every flush
// batch, so the current channel's rows were never sent.
func TestOldBindingOutputDoesNotStarveFlush(t *testing.T) {
	r := newRig(t)
	// Keys that sort before every current output, so the old ones come first.
	for i := 0; i < flushBatch; i++ {
		evt := nostr.Event{CreatedAt: nostr.Timestamp(r.now.Unix()), Kind: KindDM, Tags: r.c.rowTags("", ""), Content: "old reply"}
		if err := r.c.prepare(fmt.Sprintf("a-old/%02d", i), evt); err != nil {
			t.Fatal(err)
		}
	}
	moved := r.b
	moved.Channel = "dm-2"
	c2 := r.carrier(t, moved)
	if err := c2.Ingest(ownerEvent(t, r.owner, moved.Channel, "/inspect", r.now)); err != nil {
		t.Fatal(err)
	}
	sent := 0
	if err := c2.Flush(context.Background(), func(context.Context, nostr.Event) error { sent++; return nil }, nil); err != nil {
		t.Fatal(err)
	}
	if sent != 1 {
		t.Fatalf("sent %d current output(s), want 1", sent)
	}
}
