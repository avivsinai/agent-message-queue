package buzzio

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// These tests run the real endpoint with the fake runtime.

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

// Regression: #866, an interruption between a busy rejection and its settlement
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

// Regression: #866, an interruption after admission but before the receipt stranded
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

// Regression: #866, a settlement recorded before a failed answer left /inspect settled
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
	// The failure came after admission: the command is claimed, not settled.
	if _, claimed, err := r.l.ClaimFor(dm.ID.Hex()); err != nil || !claimed {
		t.Fatalf("first delivery left no claim: %v", err)
	}
	if _, settled, err := r.l.Settled(dm.ID.Hex()); err != nil || settled {
		t.Fatalf("failed answer settled the command: settled=%v err=%v", settled, err)
	}
	r.c.grant = grant
	if err := r.c.Ingest(dm); err != nil {
		t.Fatal(err)
	}
	if pending, _ := r.l.Pending(); len(pending) != 1 {
		t.Fatalf("owed answers = %d, want the recovered answer", len(pending))
	}
}

// Regression: #866, output prepared under one relay was sent after the share moved to
// another relay with the same body and channel.
func TestOutputIsNotSentUnderAnotherRelay(t *testing.T) {
	r := newRig(t)
	if err := r.c.Ingest(ownerEvent(t, r.owner, r.b.Channel, "/inspect", r.now)); err != nil {
		t.Fatal(err)
	}
	moved := r.b
	moved.RelayHost = "another-relay"
	sent := 0
	if err := r.carrier(t, moved).Flush(context.Background(), func(context.Context, nostr.Event) error { sent++; return nil }, nil, nil); err != nil {
		t.Fatal(err)
	}
	if sent != 0 {
		t.Fatalf("sent %d output(s) prepared under another relay", sent)
	}
}

// Regression: #866, an edit prepared just before a crash lost its receipt update, and
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
	}, nil, nil)
	if len(edits) != 2 || edits[1].CreatedAt <= edits[0].CreatedAt {
		t.Fatalf("edits = %d, want two strictly later edits", len(edits))
	}
}

// Regression: #866, native_session_id was added to amq.remote.session/1, whose frozen
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

// Regression: #866, /inspect answered for a replacement native session attached after
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
	if len(pending) != 1 {
		t.Fatalf("owed answers = %d, want the one refusal", len(pending))
	}
	var evt nostr.Event
	_ = json.Unmarshal(pending[0].Event, &evt)
	if evt.Content != protocol.InertInline(ErrNotShared.Error()) {
		t.Fatalf("answered a replacement session: %q", evt.Content)
	}
}

// Regression: #866, sixteen owed answers from an earlier channel filled every flush
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
	if err := c2.Flush(context.Background(), func(context.Context, nostr.Event) error { sent++; return nil }, nil, nil); err != nil {
		t.Fatal(err)
	}
	if sent != 1 {
		t.Fatalf("sent %d current output(s), want 1", sent)
	}
}

// Regression: #866, /cancel sent request.cancel without target, epoch and
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

// Regression: #866, a crash after the root row was prepared but before its
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
	if err := c.Flush(context.Background(), func(_ context.Context, evt nostr.Event) error { sent = append(sent, evt); return nil }, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 || sent[0].Kind != KindDM || sent[1].Kind != KindEdit || tagValue(sent[1], "e") != sent[0].ID.Hex() {
		t.Fatalf("sent = %+v, want the one recovered root then its edit", sent)
	}
}

// Regression: #866, a refusal the core never stored, already shown to the
// owner, was resubmitted on redelivery once core get returned not_found.
// The interruption hits PutReceipt after the root row: at the first
// receipts-dir sync only the prepared root holds the decision; at the
// fourth the receipt names the root.
func TestShownRefusalIsNotResubmitted(t *testing.T) {
	for name, crashAt := range map[string]int{"root prepared, receipt without it": 1, "receipt names the root": 4} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.rt.WithEvidence(&protocol.Evidence{Submit: "submitted", Completion: "run_terminal"})
			dm := ownerEvent(t, r.owner, r.b.Channel, "execute this", r.now)
			stop := errors.New("interrupted while the root row's receipt was written, before settlement")
			var restore func()
			r.c.handle = func(cmd *protocol.Command, src core.Source) (any, error) {
				out, err := r.ep.Handle(cmd, src)
				if cmd.Op == protocol.OpRequestSubmit {
					reply, ok := out.(protocol.Reply)
					if err != nil || !ok || reply.Snapshot.Code != protocol.CodeUnsupported {
						t.Fatalf("expected unsupported reply, got %+v %v", out, err)
					}
					syncs := 0
					restore = fsq.SyncDirAmbientSwapForTest(func(dir string) error {
						// Production PutReceipt writes row map then ref receipt, each
						// with a directory sync before and after rename.
						if dir == filepath.Join(r.l.dir, "receipts") {
							syncs++
							if syncs == crashAt {
								panic(stop)
							}
						}
						f, err := os.Open(dir)
						if err != nil {
							return err
						}
						defer func() { _ = f.Close() }()
						return f.Sync()
					})
				}
				return out, err
			}
			func() {
				defer func() {
					if restore != nil {
						restore()
					}
					if got := recover(); got != stop {
						t.Fatalf("interruption=%v, want boundary after prepared row", got)
					}
				}()
				_ = r.c.Ingest(dm)
			}()
			r.c.handle = r.ep.Handle
			if crashAt == 1 {
				ref := protocol.EncodeRef(r.c.sourceFor(dm).Host, r.b.Target, requestIDFor(r.b, dm.ID.Hex()))
				if rc, found, err := r.l.ReceiptFor(ref); err != nil || !found || rc.RootEventID != "" {
					t.Fatalf("receipt=%+v found=%v err=%v", rc, found, err)
				}
				if _, found, err := r.l.Prepared(rootKey(ref)); err != nil || !found {
					t.Fatalf("durable root=%v err=%v", found, err)
				}
			}
			if _, settled, err := r.l.Settled(dm.ID.Hex()); err != nil || settled {
				t.Fatalf("unexpected settlement: %v %v", settled, err)
			}
			sent := 0
			if err := r.c.Flush(context.Background(), func(_ context.Context, evt nostr.Event) error {
				if !strings.Contains(evt.Content, "rejected") || !strings.Contains(evt.Content, "unsupported") {
					t.Fatalf("expected stored rejection row, got %q", evt.Content)
				}
				sent++
				return nil
			}, nil, nil); err != nil {
				t.Fatal(err)
			}
			if sent != 1 {
				t.Fatalf("expected one stored refusal, sent=%d", sent)
			}
			r.rt.WithEvidence(&protocol.Evidence{Submit: "admitted", Completion: "run_terminal"})
			r.now = r.now.Add(2 * time.Second)
			if err := r.c.Ingest(dm); err != nil {
				t.Fatal(err)
			}
			if n := r.rt.Snapshot().Dispatches; n != 0 {
				t.Fatalf("same signed event dispatched %d time(s) after its unsupported refusal row was already shown", n)
			}
		})
	}
}

// Regression: #866, after the approved native session changed, a prior receipt was
// adopted and its update sent under the new binding.
func TestNativeRebindDoesNotAdoptPriorReceipt(t *testing.T) {
	r := newRig(t)
	dm := ownerEvent(t, r.owner, r.b.Channel, "old native work", r.now)
	if err := r.c.Ingest(dm); err != nil {
		t.Fatal(err)
	}
	ref := protocol.EncodeRef(r.c.sourceFor(dm).Host, r.b.Target, requestIDFor(r.b, dm.ID.Hex()))
	moved := r.b
	moved.NativeSession = "replacement"
	c2 := r.carrier(t, moved)
	c2.identity = func(string) string { return "replacement" }
	r.now = r.now.Add(2 * time.Second)
	if err := c2.Publish(protocol.Snapshot{RequestRef: ref, Revision: 999, State: protocol.StateCompleted}, r.c.sourceFor(dm).Origin); err != nil {
		return
	}
	sent := 0
	if err := c2.Flush(context.Background(), func(context.Context, nostr.Event) error { sent++; return nil }, nil, nil); err != nil {
		t.Fatal(err)
	}
	if sent != 0 {
		t.Fatalf("sent %d updates from prior native session under replacement sharing binding", sent)
	}
}
