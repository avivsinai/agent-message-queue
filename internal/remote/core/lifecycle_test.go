package core_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// TestHandleBlockedByConcurrentCloseCommits proves the drain wait: a Handle
// that dispatched to native runtime and is blocked before commit must not be
// cut off by a concurrent Close. Close waits for the handler to finish, and
// the command commits (no store_closed error).
//
// Mutation: remove the drain wait in Close -> the handler's commit hits a
// closed store, and the test fails (the reply is an error, not a snapshot).
func TestHandleBlockedByConcurrentCloseCommits(t *testing.T) {
	store, now := openStoreNoCleanup(t)
	ep := core.New(core.Config{Store: store, Now: now})
	rt := fake.New("fake", "e_1")
	ep.Register(rt)

	cmd := submitCmd("11111111-1111-4111-8111-111111111b01")

	// Hold the native Submit so Handle blocks after registering in-flight.
	rt.HoldAdmission()

	// An early t.Fatal between Hold and the explicit release must
	// not leave the gate held (goroutine leak under -race). Idempotent:
	// release no-ops once the gate channel is closed.
	t.Cleanup(rt.ReleaseAdmission)
	handleDone := make(chan struct{})
	var reply any
	var handleErr error
	go func() {
		defer close(handleDone)
		reply, handleErr = ep.Handle(cmd, core.Source{Host: "local"})
	}()

	// Wait for the handler to be in-flight: the fake's Submit was called.
	// We know it reached Submit because HoldAdmission gates inside Submit.
	// The admissionGate channel exists once HoldAdmission was called; we
	// need to know the handler actually ENTERED Submit. Poll the fake's
	// in-flight state.
	waitForInFlight(t, ep)

	// Start Close concurrently while the handler is blocked.
	closeDone := make(chan error)
	go func() {
		closeDone <- ep.Close()
	}()

	// Release the handler so it can proceed to commit.
	rt.ReleaseAdmission()

	// The handler must commit successfully (no store_closed).
	select {
	case <-handleDone:
	case <-time.After(10 * time.Second):
		t.Fatal("handle did not return within 10s — drain wait deadlocked")
	}
	if handleErr != nil {
		t.Fatalf("handle returned error after concurrent close: %v", handleErr)
	}
	reply2, ok := reply.(protocol.Reply)
	if !ok {
		t.Fatalf("handle did not return a Reply: %T", reply)
	}
	if reply2.Snapshot.State != protocol.StateRunning {
		t.Fatalf("expected state running, got %s", reply2.Snapshot.State)
	}

	// Close must return without error (in-flight reached zero before bound).
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("close returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("close did not return within 10s")
	}
}

// TestHandleDuringDrainingRefusedWithActionRequiredCode proves the entry
// check: a Handle arriving AFTER Close has started the drain is refused with
// CodeDraining (an action-required code), never a failure code that would be
// recorded as the command's outcome.
//
// Mutation: remove the state check in Handle -> the command is accepted
// during draining, and the test fails (no refusal error).
func TestHandleDuringDrainingRefusedWithActionRequiredCode(t *testing.T) {
	store, now := openStoreNoCleanup(t)
	ep := core.New(core.Config{Store: store, Now: now})
	rt := fake.New("fake", "e_1")
	ep.Register(rt)

	cmd := submitCmd("11111111-1111-4111-8111-111111111b02")

	// Hold one handler so Close enters the drain wait (in-flight > 0).
	rt.HoldAdmission()

	// An early t.Fatal between Hold and the explicit release must
	// not leave the gate held (goroutine leak under -race). Idempotent:
	// release no-ops once the gate channel is closed.
	t.Cleanup(rt.ReleaseAdmission)
	blockerDone := make(chan struct{})
	go func() {
		defer close(blockerDone)
		_, _ = ep.Handle(cmd, core.Source{Host: "local"})
	}()
	waitForInFlight(t, ep)

	// Start Close — it transitions to draining and blocks on the in-flight
	// handler.
	closeDone := make(chan error)
	go func() {
		closeDone <- ep.Close()
	}()

	waitForDraining(t, ep)

	// A second Handle arriving during draining must be refused with
	// CodeDraining.
	cmd2 := submitCmd("11111111-1111-4111-8111-111111111b03")
	_, err := ep.Handle(cmd2, core.Source{Host: "local"})
	if err == nil {
		t.Fatal("handle during draining was not refused")
	}
	var r *protocol.Refusal
	if !errors.As(err, &r) {
		t.Fatalf("handle during draining returned non-refusal error: %v", err)
	}
	if r.Code != protocol.CodeDraining {
		t.Fatalf("expected CodeDraining, got %s (message: %s)", r.Code, r.Message)
	}

	// Release the blocker so Close can finish.
	rt.ReleaseAdmission()
	select {
	case <-blockerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("blocked handler did not return")
	}
	select {
	case <-closeDone:
	case <-time.After(10 * time.Second):
		t.Fatal("close did not return after blocker released")
	}
}

// TestCloseBoundedWithWedgedHandler proves the drain timeout: a handler
// that never returns must not block Close forever. Close returns within the
// bound and reports how many handlers were still in flight.
//
// Mutation: remove the bound (make Close wait forever) -> the test fails on
// its 10s hard deadline.
func TestCloseBoundedWithWedgedHandler(t *testing.T) {
	store, now := openStoreNoCleanup(t)
	ep := core.New(core.Config{Store: store, Now: now, DrainTimeout: 200 * time.Millisecond})
	rt := fake.New("fake", "e_1")
	ep.Register(rt)

	cmd := submitCmd("11111111-1111-4111-8111-111111111b04")

	// Hold the handler forever (never release during the test). Clean up
	// after Close returns so the goroutine does not leak.
	rt.HoldAdmission()

	// An early t.Fatal between Hold and the explicit release must
	// not leave the gate held (goroutine leak under -race). Idempotent:
	// release no-ops once the gate channel is closed.
	t.Cleanup(rt.ReleaseAdmission)
	go func() {
		_, _ = ep.Handle(cmd, core.Source{Host: "local"})
	}()
	waitForInFlight(t, ep)

	// Close must return within a bound and report the in-flight count.
	start := time.Now()
	err := ep.Close()
	elapsed := time.Since(start)

	// Release the wedged handler so the goroutine can exit (the store is
	// closed; its commit will fail silently).
	rt.ReleaseAdmission()
	// Wait for the handler goroutine to finish so it does not race with
	// the test cleanup's store.Close. The handler's commit will fail with
	// store_closed; we discard the error.
	handlerReturned := make(chan struct{})
	go func() {
		// The original goroutine is still in Submit; once released it will
		// proceed through finishAdmissionLocked and return. We just need
		// to give it time. The goroutine was started above; this is a
		// sentinel we poll for via InFlight reaching zero.
		for ep.InFlight() > 0 {
			time.Sleep(time.Millisecond)
		}
		close(handlerReturned)
	}()
	select {
	case <-handlerReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("wedged handler did not exit after release")
	}

	// The bound is drainTO (200ms in this test). If the bound were removed,
	// Close would block forever and the test would hit the 10s deadline.
	if elapsed > 5*time.Second {
		t.Fatalf("close took %s — drain timeout was not enforced", elapsed)
	}

	if err == nil {
		t.Fatal("expected close to report in-flight handlers, got nil error")
	}
	// The error must name the count.
	if !strings.Contains(err.Error(), "1 handler") {
		t.Fatalf("error does not report in-flight count: %v", err)
	}
}

// waitForInFlight blocks until the endpoint has at least one in-flight
// handler, with a hard deadline. This replaces a blind sleep: it observes
// the actual state, not a guessed delay.
func waitForInFlight(t *testing.T, ep *core.Endpoint) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ep.InFlight() > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for handler to enter in-flight state")
}

// openStoreNoCleanup creates a store without registering a t.Cleanup that
// calls store.Close. Tests that call ep.Close() (which closes the store)
// use this to avoid a double-close race on the store's bare closed flag.
func openStoreNoCleanup(t *testing.T) (*requests.Store, func() time.Time) {
	clk := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	now := func() time.Time { return clk }
	store, err := requests.Open(t.TempDir(), requests.WithClock(now))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return store, now
}

// TestTickDuringDrainingDoesNotStartNewWork reproduces #782: Reconcile/Tick were not lifecycle-gated and
// Close waited only on Handle's inFlight. A Tick racing Close could begin new
// native work (admitDeferred -> att.Submit) after the endpoint had begun
// draining — shutdown starting work it would not finish.
//
// The fix gates Reconcile/Tick on lifecycle state (beginReconcile: if state
// != stateAccepting, skip) and registers the sweep as in-flight so Close's
// drain wait bounds it. This test proves a Tick that arrives AFTER Close has
// transitioned to draining does NOT call att.Submit (no new native work).
//
// Mutation RED: remove the beginReconcile gate in Reconcile -> the Tick
// proceeds during draining and calls worker.Submit (dispatches > 0), and the
// test fails.
func TestTickDuringDrainingDoesNotStartNewWork(t *testing.T) {
	store, now := openStoreNoCleanup(t)
	ep := core.New(core.Config{Store: store, Now: now})

	// "blocker" target: a Handle on it will admit then block after admission,
	// keeping inFlight > 0 so Close lingers in draining.
	blocker := fake.New("blocker", "e_1")
	ep.Register(blocker)
	// "worker" target: holds a deferred (StateReceived) record that Tick would
	// admit via admitDeferred -> worker.Submit. Asserting worker dispatches
	// stays 0 proves the gate refused the Tick.
	worker := fake.New("worker", "e_1")
	ep.Register(worker)

	// Seed a deferred record for the worker target. admitDeferred will Submit
	// it when Tick runs (no sibling in flight for "worker").
	seedID := "11111111-1111-4111-8111-111111111471"
	seed := &requests.Record{Snapshot: protocol.Snapshot{
		Schema: protocol.SchemaRequest, RequestID: seedID, CreatorHost: "local",
		TargetID: "worker", Epoch: "e_1", Revision: 1, State: protocol.StateReceived,
		InputDigest: requests.Digest([]byte("work")), ObservedAt: protocol.FormatTime(now()),
		NotAfter: protocol.FormatTime(now().Add(2 * time.Minute)),
	}, Input: &protocol.SubmitInput{Text: "work"}}
	if err := store.Create(seed); err != nil {
		t.Fatalf("seed received record: %v", err)
	}

	// Hold the blocker Handle AFTER admission so it lingers in-flight.
	blocker.HoldAfterAdmit()
	// Ensure held operations are released on failure too.
	t.Cleanup(blocker.ReleaseAfterAdmit)
	blockerCmd := &protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit,
		RequestID: "11111111-1111-4111-8111-111111111472", TargetID: "blocker", Epoch: "e_1",
		NotAfter: protocol.FormatTime(now().Add(2 * time.Minute)),
		Input:    &protocol.SubmitInput{Text: "block"},
	}
	handleDone := make(chan struct{})
	go func() {
		defer close(handleDone)
		_, _ = ep.Handle(blockerCmd, core.Source{Host: "local"})
	}()
	waitForInFlight(t, ep) // blocker Handle is now in-flight (admitted, blocked after)

	// Start Close. It transitions to draining and waits for the in-flight
	// blocker Handle to drain.
	closeDone := make(chan error, 1)
	go func() { closeDone <- ep.Close() }()

	// Synchronize on the ACTUAL lifecycle transition: poll IsDraining() until
	// Close has set state = draining. No elapsed-time sleep.
	waitForDraining(t, ep)

	// A Tick arriving during draining must NOT begin new native work.
	before := worker.Snapshot().Dispatches
	if err := ep.Tick(); err != nil {
		t.Fatalf("tick during draining returned error: %v", err)
	}
	after := worker.Snapshot().Dispatches
	if after != before {
		t.Fatalf("tick during draining started new native work: worker dispatches %d -> %d (Tick must be gated on lifecycle state)", before, after)
	}

	// Release the blocker so Close can finish.
	blocker.ReleaseAfterAdmit()
	select {
	case <-handleDone:
	case <-time.After(10 * time.Second):
		t.Fatal("blocker handle did not return")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("close returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("close did not return after blocker released")
	}
}

// TestTickRegistersInFlightForDrain proves the second half of #782:
// when a Tick IS accepted (state accepting), it registers as in-flight so
// Close's drain wait bounds it. A Close racing an in-flight Tick must observe
// inFlight > 0 and wait (not close the store out from under the sweep).
//
// Mutation RED: remove the inFlight++ in beginReconcile (but keep the state
// check) -> Close observes inFlight == 0, closes the store, and the in-flight
// Tick's store.List/store.Get hits a closed store (the Tick returns an error
// or, worse, the sweep races the close).
func TestTickRegistersInFlightForDrain(t *testing.T) {
	store, now := openStoreNoCleanup(t)
	ep := core.New(core.Config{Store: store, Now: now})
	worker := fake.New("worker", "e_1")
	ep.Register(worker)

	// Hold Lookup so the Tick's reconcileLive call blocks inside the
	// attachment, keeping the sweep in-flight long enough to observe.
	worker.HoldLookup()

	// An early t.Fatal between Hold and the explicit release must
	// not leave the gate held (goroutine leak under -race). Idempotent:
	// release no-ops once the gate channel is closed.
	t.Cleanup(worker.ReleaseLookup)

	// Seed a non-terminal record so reconcileLive has something to Lookup.
	// Create as Received (the only non-terminal seed the store accepts for a
	// new record) then transition to Dispatching so reconcileLive runs.
	seedID := "11111111-1111-4111-8111-111111111473"
	seed := &requests.Record{Snapshot: protocol.Snapshot{
		Schema: protocol.SchemaRequest, RequestID: seedID, CreatorHost: "local",
		TargetID: "worker", Epoch: "e_1", Revision: 1, State: protocol.StateReceived,
		InputDigest: requests.Digest([]byte("work")), ObservedAt: protocol.FormatTime(now()),
		NotAfter: protocol.FormatTime(now().Add(2 * time.Minute)),
	}}
	if err := store.Create(seed); err != nil {
		t.Fatalf("seed received record: %v", err)
	}
	seed.Revision = 2
	seed.State = protocol.StateDispatching
	seed.NativeDispatches = 1
	if err := store.Update(seed); err != nil {
		t.Fatalf("transition seed to dispatching: %v", err)
	}

	tickDone := make(chan struct{})
	go func() {
		defer close(tickDone)
		_ = ep.Tick()
	}()

	// Wait for the Tick to register as in-flight. The sweep blocks inside
	// worker.Lookup (held), so inFlight must be >= 1 while the Tick runs.
	waitForInFlight(t, ep)

	// A concurrent Close must see the in-flight Tick (inFlight > 0) and enter
	// the drain wait rather than closing immediately.
	closeDone := make(chan error, 1)
	go func() { closeDone <- ep.Close() }()

	// Synchronize on the drain wait: Close has entered draining AND the Tick
	// is still in-flight (Lookup held). Close must be blocked in the drain
	// wait, NOT returned. Verify by observing IsDraining() while the held
	// Lookup keeps the Tick in-flight — if Close returned before the Tick
	// drained, the gate did not register the Tick.
	waitForDraining(t, ep)
	if ep.InFlight() == 0 {
		t.Fatal("close drained with no in-flight tick (Tick must register as in-flight for Close to drain)")
	}

	// Release the held Lookup so the Tick can complete and Close can drain.
	worker.ReleaseLookup()
	select {
	case <-tickDone:
	case <-time.After(10 * time.Second):
		t.Fatal("tick did not return after lookup released")
	}
	select {
	case <-closeDone:
	case <-time.After(10 * time.Second):
		t.Fatal("close did not return after tick drained")
	}
}

// waitForDraining polls IsDraining() until the endpoint has begun shutdown.
// Replaces an elapsed-time sleep with synchronization on the actual lifecycle
// transition.
func waitForDraining(t *testing.T, ep *core.Endpoint) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ep.IsDraining() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for endpoint to enter draining")
}
