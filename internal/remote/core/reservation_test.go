package core_test

import (
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// TestEndpointReservationRefusesSecondConcurrentDispatch pins the B14
// per-runtime reservation: while request A is in flight (running) at the
// endpoint, a second submit to the same target is refused busy by the
// ENDPOINT — before any second native dispatch — regardless of what the
// attachment's Inspect() reports. The reservation is endpoint-owned state
// (dispatching/running/uncertain), not a race on the attachment's status.
func TestEndpointReservationRefusesSecondConcurrentDispatch(t *testing.T) {
	store, now := openStore(t)
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: store, Now: now})
	ep.Register(rt)

	// A: admitted, running.
	idA := "11111111-1111-4111-8111-1111111111a1"
	if _, err := ep.Handle(submitCmd(idA), core.Source{Host: "local"}); err != nil {
		t.Fatalf("submit A: %v", err)
	}
	before := rt.Snapshot().Dispatches

	// B: must be refused busy by the endpoint, with no second dispatch. The
	// refusal travels as an unpersisted outcome — nothing durable for a
	// submit that never dispatched (the store keeps a `received` dedup
	// placeholder so a plain retry remains possible).
	idB := "11111111-1111-4111-8111-1111111111b2"
	repAny, err := ep.Handle(submitCmd(idB), core.Source{Host: "local"})
	if err != nil {
		t.Fatalf("submit B returned an error instead of an outcome: %v", err)
	}
	rep, _ := repAny.(protocol.Reply)
	if rep.Outcome.Code != protocol.CodeBusy {
		t.Fatalf("submit B outcome code = %q, want busy", rep.Outcome.Code)
	}
	if recB, ok, gerr := store.Get(requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: idB}); gerr == nil && ok && recB.State == protocol.StateRejected {
		t.Fatal("refused submit persisted a rejected record")
	}
	if got := rt.Snapshot().Dispatches; got != before {
		t.Fatalf("second dispatch happened: %d -> %d", before, got)
	}

	// Once A resolves, B's identical retry dispatches normally.
	rt.Complete(idA, "done")
	time.Sleep(50 * time.Millisecond)
	if _, err := ep.Handle(submitCmd(idB), core.Source{Host: "local"}); err != nil {
		t.Fatalf("retry B after A resolved: %v", err)
	}
	if got := rt.Snapshot().Dispatches; got != before+1 {
		t.Fatalf("retry B did not dispatch: %d -> %d", before, got)
	}
}
