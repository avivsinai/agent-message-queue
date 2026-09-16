package core_test

import (
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// TestB47TickDuringDrainingDoesNotStartNewWork reproduces
// agent-message-queue-611.22.47: Reconcile/Tick were not lifecycle-gated and
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
func TestB47TickDuringDrainingDoesNotStartNewWork(t *testing.T) {
	store, now := openStoreNoCleanup(t)
	ep := core.New(core.Config{Store: store, Now: now})

	// "blocker" target: a Handle on it will admit then block after admission,
	// keeping inFlight > 0 so Close lingers in draining.
	blocker := fake.New("blocker", "e_1")
	ep.Register(blocker)
	// "worker" target: holds a deferred (StateReceived) record that Tick would
	// admit via admitDeferred -> worker.Submit. Asserting worker.dispatches
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

	// Yield so Close acquires e.mu and sets state = draining before the Tick.
	// Correctness does not depend on the yield length: with the gate, a Tick
	// at ANY point after draining begins is refused; without the gate, a Tick
	// during draining proceeds and the assertion fails.
	time.Sleep(50 * time.Millisecond)

	// A Tick arriving during draining must NOT begin new native work.
	before := worker.Snapshot().Dispatches
	if err := ep.Tick(); err != nil {
		t.Fatalf("tick during draining returned error: %v", err)
	}
	after := worker.Snapshot().Dispatches
	if after != before {
		t.Fatalf("tick during draining started new native work: worker dispatches %d -> %d (611.22.47: Tick must be gated on lifecycle state)", before, after)
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

// TestB47TickRegistersInFlightForDrain proves the second half of 611.22.47:
// when a Tick IS accepted (state accepting), it registers as in-flight so
// Close's drain wait bounds it. A Close racing an in-flight Tick must observe
// inFlight > 0 and wait (not close the store out from under the sweep).
//
// Mutation RED: remove the inFlight++ in beginReconcile (but keep the state
// check) -> Close observes inFlight == 0, closes the store, and the in-flight
// Tick's store.List/store.Get hits a closed store (the Tick returns an error
// or, worse, the sweep races the close).
func TestB47TickRegistersInFlightForDrain(t *testing.T) {
	store, now := openStoreNoCleanup(t)
	ep := core.New(core.Config{Store: store, Now: now})
	worker := fake.New("worker", "e_1")
	ep.Register(worker)

	// Hold Lookup so the Tick's reconcileLive call blocks inside the
	// attachment, keeping the sweep in-flight long enough to observe.
	worker.HoldLookup()
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

	// Close should be blocked in the drain wait (Tick still in-flight). Give
	// it a moment; it must NOT have returned yet.
	select {
	case <-closeDone:
		// If Close returned while the Tick is still in-flight (Lookup held),
		// the gate did not register the Tick for draining.
		t.Fatal("close returned before the in-flight tick drained (611.22.47: Tick must register as in-flight for Close to drain)")
	case <-time.After(100 * time.Millisecond):
		// expected: Close is waiting in the drain wait.
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
