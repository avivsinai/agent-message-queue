package core_test

import (
	"errors"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// cancelCmd builds a cancel command for a local-host record.
func cancelCmd(id string) *protocol.Command {
	return &protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpRequestCancel,
		RequestRef: protocol.EncodeRef("local", "fake", id),
		TargetID:   "fake", Epoch: "e_1",
		NotAfter: protocol.FormatTime(time.Date(2026, 9, 8, 10, 2, 0, 0, time.UTC)),
	}
}

// submitWait submits and waits for the reply.
func submitWait(ep *core.Endpoint, id string) error {
	_, err := ep.Handle(submitCmd(id), core.Source{Host: "local"})
	return err
}

// b14aReconcileWithTimeout runs one Reconcile under a hard deadline: a
// lock-leak regression FAILS here instead of hanging CI.
func b14aReconcileWithTimeout(ep *core.Endpoint) error {
	done := make(chan error, 1)
	go func() { done <- ep.Reconcile() }()
	select {
	case err := <-done:
		return err
	case <-time.After(b14aDeadline):
		return errors.New("reconcile did not return within deadline — lock leak regression")
	}
}

// newB14aEndpoint builds an endpoint over a store with a registered fake
// runtime, ready for reconcile-path tests.
func newB14aEndpoint(t *testing.T) (*core.Endpoint, *fake.Runtime, *requests.Store) {
	t.Helper()
	base, now := openStore(t)
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: base, Now: now})
	ep.Register(rt)
	t.Cleanup(func() { _ = ep.Close() })
	return ep, rt, base
}

// b14aDeadline bounds every blocking wait in these tests: a regression
// back to a self-deadlock must FAIL (deadline tripped), not hang CI.
const b14aDeadline = 5 * time.Second

// b14aHandleWithTimeout runs one Handle under the same hard deadline, so a
// wedged endpoint FAILS instead of hanging CI.
func b14aHandleWithTimeout(ep *core.Endpoint, cmd *protocol.Command) (any, error) {
	type reply struct {
		v   any
		err error
	}
	done := make(chan reply, 1)
	go func() {
		v, err := ep.Handle(cmd, core.Source{Host: "local"})
		done <- reply{v, err}
	}()
	select {
	case r := <-done:
		return r.v, r.err
	case <-time.After(b14aDeadline):
		return nil, errors.New("handle did not return within deadline — endpoint wedged")
	}
}

// TestB14aReconcileUnregisteredTargetNoDeadlock exercises reconcileLive's
// uncertain sub-branch with the target UNREGISTERED — the normal restart
// shape that deadlocked B14 before the recut (#1a: return without Unlock).
// With the bug, Reconcile re-locks e.mu and the whole endpoint wedges; this
// test times out instead of hanging.
func TestB14aReconcileUnregisteredTargetNoDeadlock(t *testing.T) {
	store, now := openStore(t)
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: store, Now: now})
	ep.Register(rt)
	t.Cleanup(func() { _ = ep.Close() })

	id := "11111111-1111-4111-8111-1111111111a1"
	done := make(chan error, 1)
	go func() { done <- submitWait(ep, id) }()

	// Dispatch, then unregister the target so Lookup is skipped: reconcileLive
	// hits the `!ok` uncertain sub-branch with an uncertain record — the exact
	// early return that leaked the lock.
	if !b14cWait(func() bool { return rt.Snapshot().Dispatches == 1 }) {
		t.Fatal("request never dispatched")
	}
	ep.UnregisterAll()
	if err := <-done; err != nil {
		t.Fatalf("submit errored: %v", err)
	}
	// Reconcile now: first pass flips the record to uncertain via the `!ok`
	// branch (previously: self-deadlock here).
	if err := b14aReconcileWithTimeout(ep); err != nil {
		t.Fatalf("reconcile after unregister: %v", err)
	}
	rec, ok, err := store.Get(requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id})
	if err != nil || !ok {
		t.Fatalf("record gone: ok=%v err=%v", ok, err)
	}
	if rec.State != protocol.StateUncertain {
		t.Fatalf("state = %s, want uncertain after target lost", rec.State)
	}
	// Second reconcile: same branch again with rec.State already uncertain —
	// the exact `if rec.State == StateUncertain { return }` leak site.
	if err := b14aReconcileWithTimeout(ep); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	// The endpoint must still be live: Tick returns and the reply path works.
	if err := b14aReconcileWithTimeout(ep); err != nil {
		t.Fatalf("endpoint wedged after uncertain reconcile: %v", err)
	}
}

// TestB14aReconcileLookupErrorNoDeadlock drives reconcileLive through the
// `lookupErr != nil` sub-branch (fake Lookup now errors) — the other leaked
// early return in #1a — and through a store.Update failure in the reconcile
// body (#1b), each under a hard deadline.
func TestB14aReconcileLookupErrorNoDeadlock(t *testing.T) {
	ep, rt, _ := newB14aEndpoint(t)

	if _, err := ep.Handle(submitCmd("11111111-1111-4111-8111-1111111111a1"), core.Source{Host: "local"}); err != nil {
		t.Fatal(err)
	}
	if !b14cWait(func() bool { return rt.Snapshot().Dispatches == 1 }) {
		t.Fatal("request never dispatched")
	}
	rt.FailNextLookup(errors.New("fake lookup down"))
	if err := b14aReconcileWithTimeout(ep); err != nil {
		t.Fatalf("reconcile with Lookup error: %v", err)
	}
	// Endpoint still live: Tick must return, not wedge.
	if err := b14aReconcileWithTimeout(ep); err != nil {
		t.Fatalf("second reconcile after Lookup error: %v", err)
	}
}

// TestB14aOnNativeStaleStateNoDeadlock drives onNative's
// RunCompleted/Failed/Cancelled guard with a record in `received` — the
// #1c leaked return. With the bug the endpoint wedges on the next command.
func TestB14aOnNativeStaleStateNoDeadlock(t *testing.T) {
	store, now := openStore(t)
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: store, Now: now})
	ep.Register(rt)
	t.Cleanup(func() { _ = ep.Close() })

	// A cancel-before-submit tombstone exists; now a completed event for a
	// record whose durable state is `received` (dispatching write lost) hits
	// the guard branch.
	id := "11111111-1111-4111-8111-1111111111a1"
	rec := &requests.Record{Snapshot: protocol.Snapshot{
		Schema: protocol.SchemaRequest, RequestID: id, CreatorHost: "local",
		TargetID: "fake", Epoch: "e_1", Revision: 1, State: protocol.StateReceived,
		InputDigest: requests.Digest([]byte("x")), ObservedAt: protocol.FormatTime(now()),
	}}
	if err := store.Create(rec); err != nil {
		t.Fatal(err)
	}
	rt.Complete(id, "done") // emits EventRunCompleted against a `received` record
	// The endpoint must still answer commands.
	repAny, err := b14aHandleWithTimeout(ep, cancelCmd(id))
	if err != nil {
		t.Fatalf("endpoint wedged after stale-state native event: %v", err)
	}
	if _, ok := repAny.(protocol.Reply); !ok {
		t.Fatalf("cancel reply type = %T", repAny)
	}
}

// TestB14aNativeAckUnderWedgedAttachment pins recut #2: the native ack runs
// OUTSIDE e.mu — a wedged AcknowledgeResult (here the direct ack onNative
// sends for a terminal event) must not prevent subsequent commands on the
// endpoint.
func TestB14aNativeAckUnderWedgedAttachment(t *testing.T) {
	store, now := openStore(t)
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: store, Now: now})
	ep.Register(rt)
	t.Cleanup(func() { _ = ep.Close() })

	id := "11111111-1111-4111-8111-1111111111a1"
	if _, err := ep.Handle(submitCmd(id), core.Source{Host: "local"}); err != nil {
		t.Fatal(err)
	}
	// Complete on a goroutine (Complete emits the terminal event inline and
	// onNative acks it — that ack is the one we hold). The record goes
	// terminal; a LATER Reconcile exercises replayTerminalAck only if the
	// ack did not land, so first let the direct ack get wedged.
	rt.HoldAcknowledge()
	completeDone := make(chan struct{})
	go func() { rt.Complete(id, "done"); close(completeDone) }()
	// Wait until the ack is provably stuck (fake saw the call).
	if !b14cWait(func() bool { return rt.AckCalls() >= 1 }) {
		t.Fatal("ack never reached the fake")
	}
	// While the ack is wedged, the endpoint must still serve commands.
	if _, err := b14aHandleWithTimeout(ep, cancelCmd(id)); err != nil {
		t.Fatalf("endpoint wedged while ack blocked: %v", err)
	}
	rt.ReleaseAcknowledge()
	if !b14cWait(func() bool {
		select {
		case <-completeDone:
			return true
		default:
			return false
		}
	}) {
		t.Fatal("wedged ack never returned after release")
	}
}
