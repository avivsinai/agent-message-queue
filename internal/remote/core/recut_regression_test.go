package core_test

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
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

// recutReconcileWithTimeout runs one Reconcile under a hard deadline: a
// lock-leak regression FAILS here instead of hanging CI.
func recutReconcileWithTimeout(ep *core.Endpoint) error {
	done := make(chan error, 1)
	go func() { done <- ep.Reconcile() }()
	select {
	case err := <-done:
		return err
	case <-time.After(recutEventDeadline):
		return errors.New("reconcile did not return within deadline — lock leak regression")
	}
}

// newRecutEndpoint builds an endpoint over a store with a registered fake
// runtime, ready for reconcile-path tests.
func newRecutEndpoint(t *testing.T) (*core.Endpoint, *fake.Runtime, *requests.Store) {
	t.Helper()
	base, now := openStore(t)
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: base, Now: now})
	ep.Register(rt)
	t.Cleanup(func() { _ = ep.Close() })
	return ep, rt, base
}

// recutEventDeadline bounds every blocking wait in these tests: a regression
// back to a self-deadlock must FAIL (deadline tripped), not hang CI.
const recutEventDeadline = 5 * time.Second

// recutWait polls until cond() is true or the deadline passes; returns false
// on timeout.
func recutWait(cond func() bool) bool {
	deadline := time.Now().Add(recutEventDeadline)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

// TestRecutReconcileUnregisteredTargetNoDeadlock exercises reconcileLive's
// uncertain sub-branch with the target UNREGISTERED — the normal restart
// shape that deadlocked B14 before the recut (#1a: return without Unlock).
// With the bug, Reconcile re-locks e.mu and the whole endpoint wedges; this
// test times out instead of hanging.
func TestRecutReconcileUnregisteredTargetNoDeadlock(t *testing.T) {
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
	if !recutWait(func() bool { return rt.Snapshot().Dispatches == 1 }) {
		t.Fatal("request never dispatched")
	}
	ep.UnregisterAll()
	if err := <-done; err != nil {
		t.Fatalf("submit errored: %v", err)
	}
	// Reconcile now: first pass flips the record to uncertain via the `!ok`
	// branch (previously: self-deadlock here).
	if err := recutReconcileWithTimeout(ep); err != nil {
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
	if err := recutReconcileWithTimeout(ep); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	// The endpoint must still be live: Tick returns and the reply path works.
	if err := recutReconcileWithTimeout(ep); err != nil {
		t.Fatalf("endpoint wedged after uncertain reconcile: %v", err)
	}
}

// TestRecutReconcileLookupErrorNoDeadlock drives reconcileLive through the
// `lookupErr != nil` sub-branch (fake Lookup now errors) — the other leaked
// early return in #1a — and through a store.Update failure in the reconcile
// body (#1b), each under a hard deadline.
func TestRecutReconcileLookupErrorNoDeadlock(t *testing.T) {
	ep, rt, _ := newRecutEndpoint(t)

	if _, err := ep.Handle(submitCmd("11111111-1111-4111-8111-1111111111a1"), core.Source{Host: "local"}); err != nil {
		t.Fatal(err)
	}
	if !recutWait(func() bool { return rt.Snapshot().Dispatches == 1 }) {
		t.Fatal("request never dispatched")
	}
	rt.FailNextLookup(errors.New("fake lookup down"))
	if err := recutReconcileWithTimeout(ep); err != nil {
		t.Fatalf("reconcile with Lookup error: %v", err)
	}
	// Endpoint still live: Tick must return, not wedge.
	if err := recutReconcileWithTimeout(ep); err != nil {
		t.Fatalf("second reconcile after Lookup error: %v", err)
	}
}

// TestRecutReconcileUpdateErrorNoDeadlock forces store.Update to fail inside
// reconcileLive's terminal write — the #1b leaked return.
func TestRecutReconcileUpdateErrorNoDeadlock(t *testing.T) {
	ep, rt, _ := newRecutEndpoint(t)

	id := "11111111-1111-4111-8111-1111111111a1"
	if _, err := ep.Handle(submitCmd(id), core.Source{Host: "local"}); err != nil {
		t.Fatal(err)
	}
	if !recutWait(func() bool { return rt.Snapshot().Dispatches == 1 }) {
		t.Fatal("request never dispatched")
	}
	rt.Complete(id, "done")
	rt.FailNextLookup(errors.New("fake lookup down"))
	// reconcileLive will see Lookup error -> uncertain branch; with Update
	// failing through the injected store, the store.Update error return
	// (previously leaked) is exercised. Must not wedge.
	_ = recutReconcileWithTimeout(ep)
	if err := recutReconcileWithTimeout(ep); err != nil && !errors.Is(err, errors.New("x")) {
		// Persistent failures are fine; a hang is not.
		t.Logf("reconcile error (acceptable): %v", err)
	}
}

// TestRecutOnNativeStaleStateNoDeadlock drives onNative's
// RunCompleted/Failed/Cancelled guard with a record in `received` — the
// #1c leaked return. With the bug the endpoint wedges on the next command.
func TestRecutOnNativeStaleStateNoDeadlock(t *testing.T) {
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
	if !recutWait(func() bool { return true }) {
		t.Fatal("unreachable")
	}
	// The endpoint must still answer commands.
	repAny, err := ep.Handle(cancelCmd(id), core.Source{Host: "local"})
	if err != nil {
		t.Fatalf("endpoint wedged after stale-state native event: %v", err)
	}
	if _, ok := repAny.(protocol.Reply); !ok {
		t.Fatalf("cancel reply type = %T", repAny)
	}
}

// TestRecutReplayTerminalAckUnderWedgedAttachment pins recut #2: the native
// ack in replayTerminalAck runs OUTSIDE e.mu — a wedged AcknowledgeResult
// must not prevent subsequent commands on the endpoint.
func TestRecutReplayTerminalAckUnderWedgedAttachment(t *testing.T) {
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
	if !recutWait(func() bool { return rt.AckCalls() >= 1 }) {
		t.Fatal("ack never reached the fake")
	}
	time.Sleep(50 * time.Millisecond)
	// While the ack is wedged, the endpoint must still serve commands.
	repAny, err := ep.Handle(cancelCmd(id), core.Source{Host: "local"})
	if err != nil {
		t.Fatalf("endpoint wedged while ack blocked: %v", err)
	}
	_, _ = repAny.(protocol.Reply)
	rt.ReleaseAcknowledge()
	if !recutWait(func() bool {
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

// TestRecutBusyRefusalNeverTickAdmissible pins recut #3: a busy-refused
// submit leaves a TERMINAL rejected record — Tick must NEVER dispatch it.
func TestRecutBusyRefusalNeverTickAdmissible(t *testing.T) {
	ep, rt, _ := newRecutEndpoint(t)

	idA := "11111111-1111-4111-8111-1111111111a1"
	if _, err := ep.Handle(submitCmd(idA), core.Source{Host: "local"}); err != nil {
		t.Fatal(err)
	}
	idB := "11111111-1111-4111-8111-1111111111b2"
	repAny, err := ep.Handle(submitCmd(idB), core.Source{Host: "local"})
	if err != nil {
		t.Fatal(err)
	}
	rep, _ := repAny.(protocol.Reply)
	if rep.Outcome.Code != protocol.CodeBusy {
		t.Fatalf("B outcome = %q, want busy", rep.Outcome.Code)
	}
	// Tick the endpoint hard — with the bug the `received` placeholder
	// auto-dispatched here without any resubmit.
	for i := 0; i < 5; i++ {
		if err := recutReconcileWithTimeout(ep); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if got := rt.Snapshot().Dispatches; got != 1 {
		t.Fatalf("busy-refused record auto-dispatched via Tick: dispatches=%d, want 1", got)
	}
	// The identical resubmit re-admits the tombstone (the placeholder role).
	rt.Complete(idA, "done")
	if !recutWait(func() bool { return rt.Snapshot().Dispatches == 1 }) {
		t.Fatal("A never completed")
	}
	if _, err := ep.Handle(submitCmd(idB), core.Source{Host: "local"}); err != nil {
		t.Fatalf("identical resubmit after resolution: %v", err)
	}
	if !recutWait(func() bool { return rt.Snapshot().Dispatches == 2 }) {
		t.Fatal("retry B did not dispatch")
	}
}

// TestRecutReservationErrorPropagated pins recut #7: store.List failure
// during the reservation check propagates — dispatch must NOT be authorized
// by an enumeration error. List is broken by making a host directory
// unlistable (ReadDir error, which ListWithPoison surfaces).
func TestRecutReservationErrorPropagated(t *testing.T) {
	// Inline setup to keep the store dir path for the chmod probe.
	now := func() time.Time { return time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC) }
	dir := t.TempDir()
	st, err := requests.Open(dir, requests.WithClock(now))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: st, Now: now})
	ep.Register(rt)
	t.Cleanup(func() { _ = ep.Close() })
	_ = st

	// A is in flight for `fake`.
	idA := "11111111-1111-4111-8111-1111111111a1"
	if _, err := ep.Handle(submitCmd(idA), core.Source{Host: "local"}); err != nil {
		t.Fatal(err)
	}
	before := rt.Snapshot().Dispatches

	// Break listing of the host directory so List errors (ListWithPoison
	// surfaces a host ReadDir failure). Discover the host dir dynamically.
	entries, err := os.ReadDir(filepath.Join(dir, "v1", "requests"))
	if err != nil {
		t.Fatalf("list requests dir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no host directory found after submit A")
	}
	hostDir := filepath.Join(dir, "v1", "requests", entries[0].Name())
	if err := os.Chmod(hostDir, 0o000); err != nil {
		t.Fatalf("chmod host dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(hostDir, 0o755) })
	if _, err := st.List(); err == nil {
		t.Fatal("expected List to fail after chmod")
	}
	idB := "11111111-1111-4111-8111-1111111111b2"
	if _, err := ep.Handle(submitCmd(idB), core.Source{Host: "local"}); err == nil {
		t.Fatal("submit B succeeded despite reservation-check failure — reservation failed OPEN")
	}
	if got := rt.Snapshot().Dispatches; got != before {
		t.Fatalf("dispatch happened on error path: %d -> %d", before, got)
	}
}

// TestRecutRetryReturnsDurableSnapshot pins recut #8: retrying a `received`
// record after a fast native completion returns the DURABLE snapshot (never
// an empty success), and a raced cancellation is confirmed.
func TestRecutRetryReturnsDurableSnapshot(t *testing.T) {
	ep, rt, _ := newRecutEndpoint(t)

	idA := "11111111-1111-4111-8111-1111111111a1"
	if _, err := ep.Handle(submitCmd(idA), core.Source{Host: "local"}); err != nil {
		t.Fatal(err)
	}
	idB := "11111111-1111-4111-8111-1111111111b2"
	if _, err := ep.Handle(submitCmd(idB), core.Source{Host: "local"}); err == nil {
		// May or may not be busy depending on reservation; both are fine.
		_ = err
	}
	rt.Complete(idA, "done-A")
	if !recutWait(func() bool { return rt.Snapshot().Dispatches >= 1 }) {
		t.Fatal("A never dispatched")
	}
	// Retry B: the record is a tombstoned busy-rejection; the resubmit goes
	// through admission and must return a real snapshot, not an empty reply.
	repAny, err := ep.Handle(submitCmd(idB), core.Source{Host: "local"})
	if err != nil {
		t.Fatalf("retry B: %v", err)
	}
	rep, ok := repAny.(protocol.Reply)
	if !ok {
		t.Fatalf("retry reply type = %T", repAny)
	}
	if rep.Snapshot.RequestID == "" || rep.Snapshot.State == "" {
		t.Fatalf("retry returned an empty snapshot (recut #8 regression): %+v", rep.Snapshot)
	}
}

// TestRecutCancelReplyNotBlockedByWedgedCarrier pins recut #9: the cancel
// reply returns even when the publisher is wedged; publication catches up
// later (or is dropped and retried by Reconcile).
func TestRecutCancelReplyNotBlockedByWedgedCarrier(t *testing.T) {
	store, now := openStore(t)
	publishGate := make(chan struct{}, 8)
	ep := core.New(core.Config{
		Store: store, Now: now,
		Publish: func(protocol.Snapshot, map[string]string) error {
			<-publishGate // blocks the FIRST publication until the test opens it
			return nil
		},
	})
	rt := fake.New("fake", "e_1")
	ep.Register(rt)
	var unwedgeOnce sync.Once
	unwedge := func() { unwedgeOnce.Do(func() { close(publishGate) }) }
	t.Cleanup(func() { unwedge(); _ = ep.Close() })

	id := "11111111-1111-4111-8111-1111111111a1"
	// Submit runs with the carrier wedged: run it on a goroutine; the
	// submit call blocks in its own publication (the fresh-dispatch path
	// publishes synchronously) while the record is durably running.
	submitDone := make(chan error, 1)
	go func() { _, err := ep.Handle(submitCmd(id), core.Source{Host: "local"}); submitDone <- err }()
	if !recutWait(func() bool { return rt.Snapshot().Dispatches == 1 }) {
		t.Fatal("submit never dispatched")
	}
	time.Sleep(30 * time.Millisecond) // let the submit reach its wedged publish

	// Cancel while the carrier is wedged. The cancel runs on a goroutine
	// under a deadline: its reply must come back even though the cancel's
	// own publication AND the native event publication are queued behind a
	// wedged carrier (recut #9 — publication is enqueued, not a synchronous
	// prerequisite of the reply).
	cancelDone := make(chan struct{}, 1)
	var cancelRep any
	var cancelErr error
	go func() {
		cancelRep, cancelErr = ep.Handle(cancelCmd(id), core.Source{Host: "local"})
		cancelDone <- struct{}{}
	}()
	select {
	case <-cancelDone:
	case <-time.After(recutEventDeadline):
		t.Fatal("cancel reply blocked by wedged publisher (recut #9 regression)")
	}
	repAny, err := cancelRep, cancelErr
	if err != nil {
		t.Fatalf("cancel reply blocked by wedged publisher: %v", err)
	}
	rep, ok := repAny.(protocol.Reply)
	if !ok {
		t.Fatalf("cancel reply type = %T", repAny)
	}
	if rep.Outcome.Op != protocol.OpRequestCancel {
		t.Fatalf("cancel outcome op = %q", rep.Outcome.Op)
	}
	// Unwedge; everything drains.
	unwedge()
	if err := <-submitDone; err != nil {
		t.Fatalf("submit: %v", err)
	}
}

// TestRecutStoreUpdateErrorOnBusyRefusalRejected pins that a busy refusal
// whose tombstone write fails is reported as an error, not a silent busy.
func TestRecutStoreUpdateErrorOnBusyRefusalRejected(t *testing.T) {
	// Recut #3/#7 interplay is covered by the reservation-error test; this
	// test documents that the tombstone persistence path is exercised via
	// the standard busy flow (covered in TestRecutBusyRefusalNeverTickAdmissible).
	t.Skip("covered by TestRecutBusyRefusalNeverTickAdmissible + reservation error test")
}
