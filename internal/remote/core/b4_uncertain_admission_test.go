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

// b4UncertainInFlight drives the B4 race (agent-message-queue-611.22.35):
// Submit is held open, reconcile loses the attachment and moves the record
// to uncertain, beforeRelease arms the runtime's answer, then Submit returns.
func b4UncertainInFlight(t *testing.T, ep *core.Endpoint, rt *fake.Runtime, store *requests.Store, id string, beforeRelease func()) protocol.Reply {
	t.Helper()
	k := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
	rt.HoldAdmission()

	// Bead 7eu: an early t.Fatal between Hold and the explicit release must
	// not leave the gate held (goroutine leak under -race). Idempotent:
	// release no-ops once the gate channel is closed.
	t.Cleanup(rt.ReleaseAdmission)
	type outcome struct {
		rep protocol.Reply
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		out, err := ep.Handle(submitCmd(id), core.Source{Host: "local"})
		rep, _ := out.(protocol.Reply)
		done <- outcome{rep, err}
	}()
	if !b14cWait(func() bool {
		rec, ok, _ := store.Get(k)
		return ok && rec.State == protocol.StateDispatching
	}) {
		t.Fatal("record never reached dispatching")
	}
	// Reconcile loses the attachment while Submit is still in flight.
	rt.FailNextLookup(errors.New("attachment lost"))
	if err := ep.Tick(); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if rec, ok, _ := store.Get(k); !ok || rec.State != protocol.StateUncertain {
		t.Fatalf("precondition: state=%s, want uncertain", recState(rec, ok))
	}
	beforeRelease()
	rt.ReleaseAdmission()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("submit: %v", r.err)
		}
		return r.rep
	case <-time.After(5 * time.Second):
		t.Fatal("submit did not return after the admission gate opened")
	}
	return protocol.Reply{}
}

// TestB4UncertainRecordResolvedByDefinitiveRefusal reproduces B4 (a): a
// definitive native refusal that lands on an uncertain record must reject it
// and release the target; before the fix the record stayed uncertain and the
// per-target reservation blocked every later request.
func TestB4UncertainRecordResolvedByDefinitiveRefusal(t *testing.T) {
	ep, rt, store, _ := b14cEndpoint(t)
	id := "11111111-1111-4111-8111-1111111111b4"
	rep := b4UncertainInFlight(t, ep, rt, store, id, rt.FailNextAdmissionAfterReturn)
	if rep.Snapshot.State != protocol.StateRejected || rep.Snapshot.Code != protocol.CodeNativeError {
		t.Fatalf("reply state=%s code=%s, want rejected/native_error (B4 — a definitive refusal outranks uncertainty)", rep.Snapshot.State, rep.Snapshot.Code)
	}
	k := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
	if rec, ok, _ := store.Get(k); !ok || rec.State != protocol.StateRejected {
		t.Fatalf("stored state=%s, want rejected", recState(rec, ok))
	}
	next := "11111111-1111-4111-8111-1111111111b5"
	out2, err := ep.Handle(submitCmd(next), core.Source{Host: "local"})
	if err != nil {
		t.Fatalf("second submit: %v", err)
	}
	rep2, _ := out2.(protocol.Reply)
	if rep2.Snapshot.State != protocol.StateRunning {
		t.Fatalf("second submit state=%s code=%s, want running (B4 — the uncertain record still held the target)", rep2.Snapshot.State, rep2.Snapshot.Code)
	}
}

// TestB4UncertainRecordResolvedByAdmission reproduces B4 (b): an admission
// that lands on an uncertain record must bind the run and go running; before
// the fix the record stayed uncertain with no NativeRun to drive.
func TestB4UncertainRecordResolvedByAdmission(t *testing.T) {
	ep, rt, store, _ := b14cEndpoint(t)
	id := "11111111-1111-4111-8111-1111111111b6"
	rep := b4UncertainInFlight(t, ep, rt, store, id, func() {})
	if rep.Snapshot.State != protocol.StateRunning || rep.Snapshot.NativeRun == nil {
		t.Fatalf("reply state=%s native_run=%v, want running with a bound run (B4 — admission outranks uncertainty)", rep.Snapshot.State, rep.Snapshot.NativeRun)
	}
	k := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
	rec, ok, _ := store.Get(k)
	if !ok || rec.State != protocol.StateRunning || rec.NativeRun == nil {
		t.Fatalf("stored state=%s native_run=%v, want running with a bound run", recState(rec, ok), rec.NativeRun)
	}
	if !rt.HasRun(id) {
		t.Fatal("runtime has no run for the admitted request")
	}
}
