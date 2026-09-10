package core_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// b14cWait polls until cond() is true or the deadline passes; a regression
// must FAIL, never hang.
func b14cWait(cond func() bool) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

func b14cEndpoint(t *testing.T) (*core.Endpoint, *fake.Runtime, *requests.Store, func() time.Time) {
	t.Helper()
	store, now := openStore(t)
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: store, Now: now})
	ep.Register(rt)
	t.Cleanup(func() { _ = ep.Close() })
	return ep, rt, store, now
}

// writePoisonRecord writes an undecodable file at the on-disk path a record
// for (host, targetID, id) would occupy, so ListWithPoison isolates it and
// recovers its identity from the path.
func writePoisonRecord(t *testing.T, store *requests.Store, host, targetID, id string) string {
	t.Helper()
	p := filepath.Join(store.Dir(), "requests", host, targetID+"__"+id+".json")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatalf("mkdir poison dir: %v", err)
	}
	if err := os.WriteFile(p, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("write poison record: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(p) })
	return p
}

// TestB14cReservationRefusesSecondConcurrentDispatch pins B10: while A is in
// flight for a target, a second submit is refused busy by the ENDPOINT with
// a TERMINAL rejected+tombstoned record (never a Tick-admissible received
// placeholder), no second native dispatch, and an identical resubmit after
// A resolves is admitted through the tombstone.
func TestB14cReservationRefusesSecondConcurrentDispatch(t *testing.T) {
	ep, rt, store, _ := b14cEndpoint(t)

	idA := "11111111-1111-4111-8111-1111111111a1"
	if _, err := ep.Handle(submitCmd(idA), core.Source{Host: "local"}); err != nil {
		t.Fatalf("submit A: %v", err)
	}
	before := rt.Snapshot().Dispatches

	idB := "11111111-1111-4111-8111-1111111111b2"
	repAny, err := ep.Handle(submitCmd(idB), core.Source{Host: "local"})
	if err != nil {
		t.Fatalf("submit B returned an error instead of an outcome: %v", err)
	}
	rep, ok := repAny.(protocol.Reply)
	if !ok {
		t.Fatalf("submit B reply type = %T", repAny)
	}
	if rep.Outcome.Code != protocol.CodeBusy {
		t.Fatalf("submit B outcome code = %q, want busy", rep.Outcome.Code)
	}
	recB, found, gerr := store.Get(requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: idB})
	if gerr != nil || !found {
		t.Fatalf("busy refusal record: ok=%v err=%v", found, gerr)
	}
	if recB.State != protocol.StateRejected || recB.Code != protocol.CodeBusy || !recB.Tombstone {
		t.Fatalf("busy-refused record = %s/%s tomb=%v, want rejected/busy/tombstoned", recB.State, recB.Code, recB.Tombstone)
	}
	if got := rt.Snapshot().Dispatches; got != before {
		t.Fatalf("second dispatch happened: %d -> %d", before, got)
	}

	// Once A resolves, B's identical retry dispatches normally (the caller
	// resubmits; the busy tombstone is re-admittable — auto-retry of a
	// refused request would be queue semantics, D1-disabled in v1).
	rt.Complete(idA, "done-A")
	if !b14cWait(func() bool {
		recA, ok, err := store.Get(requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: idA})
		return err == nil && ok && recA.State == protocol.StateCompleted
	}) {
		t.Fatalf("A never completed: %d", rt.Snapshot().Dispatches)
	}
	if _, err := ep.Handle(submitCmd(idB), core.Source{Host: "local"}); err != nil {
		t.Fatalf("identical resubmit B: %v", err)
	}
	recB2, found, gerr := store.Get(requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: idB})
	if gerr != nil || !found {
		t.Fatalf("retry record: ok=%v err=%v", found, gerr)
	}
	if recB2.State != protocol.StateRunning {
		t.Fatalf("retried B state = %s, want running", recB2.State)
	}
}

// TestB14cReservationPoisonFailClosed pins the B10 poison semantics: a
// record for the TARGET whose file is unreadable makes the target's
// reservation state undeterminable, so a new submit is refused — List()
// alone would have dropped the poison diagnostics and reported idle, which
// is exactly the double-dispatch this refuses. Poison for a DIFFERENT
// target does not block.
func TestB14cReservationPoisonFailClosed(t *testing.T) {
	ep, rt, store, _ := b14cEndpoint(t)

	// A poison record for the target, before any healthy submit: the submit
	// must be refused (fail-closed), not dispatched as if the target were
	// idle.
	poisonPath := writePoisonRecord(t, store, "local", "fake", "11111111-1111-4111-8111-1111111111p1")
	idB := "11111111-1111-4111-8111-1111111111b2"
	if _, err := ep.Handle(submitCmd(idB), core.Source{Host: "local"}); err == nil {
		t.Fatal("submit succeeded despite an unreadable target record — reservation failed OPEN on poison")
	}
	if got := rt.Snapshot().Dispatches; got != 0 {
		t.Fatalf("dispatch happened on poison path: %d", got)
	}

	// Removing the poison file restores dispatch.
	if err := os.Remove(poisonPath); err != nil {
		t.Fatalf("remove poison: %v", err)
	}
	idA := "11111111-1111-4111-8111-1111111111a1"
	if _, err := ep.Handle(submitCmd(idA), core.Source{Host: "local"}); err != nil {
		t.Fatalf("submit A after poison removed: %v", err)
	}

	// A poison record for a DIFFERENT target must not block this target's
	// second dispatch refusal path. Probed on a clean third target: poison
	// on "other" is tolerated there (and, by symmetry, on "fake"); the
	// poisoned target itself stays fail-closed.
	writePoisonRecord(t, store, "local", "other", "11111111-1111-4111-8111-1111111111p2")
	rtThird := fake.New("third", "e_1")
	ep.Register(rtThird)
	idC := "11111111-1111-4111-8111-1111111111c3"
	cmd := submitCmd(idC)
	cmd.TargetID = "third"
	if _, err := ep.Handle(cmd, core.Source{Host: "local"}); err != nil {
		t.Fatalf("submit on a clean third target with unrelated poison: %v", err)
	}
}

// TestB14cReservationErrorPropagated pins recut #7: a store enumeration
// failure during the reservation check propagates — dispatch must NOT be
// authorized by an error. The error is injected through the store itself
// (chmod on the host dir), skipped when running as root where chmod is a
// no-op (claude's test nit).
func TestB14cReservationErrorPropagated(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod is a no-op for root; the fail-closed poison test covers the semantics")
	}
	now := func() time.Time { return time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC) }
	dir := t.TempDir()
	st, err := requests.Open(dir, requests.WithClock(now))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: st, Now: now})
	ep.Register(rt)
	t.Cleanup(func() { _ = ep.Close() })

	idA := "11111111-1111-4111-8111-1111111111a1"
	if _, err := ep.Handle(submitCmd(idA), core.Source{Host: "local"}); err != nil {
		t.Fatal(err)
	}
	before := rt.Snapshot().Dispatches

	entries, err := os.ReadDir(filepath.Join(dir, "v1", "requests"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("no host directory: %v", err)
	}
	hostDir := filepath.Join(dir, "v1", "requests", entries[0].Name())
	if err := os.Chmod(hostDir, 0o000); err != nil {
		t.Fatalf("chmod host dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(hostDir, 0o755) })
	if _, lerr := st.List(); lerr == nil {
		t.Fatal("expected List to fail after chmod; test precondition broken")
	}
	idB := "11111111-1111-4111-8111-1111111111b2"
	if _, err := ep.Handle(submitCmd(idB), core.Source{Host: "local"}); err == nil {
		t.Fatal("submit B succeeded despite reservation-check failure — reservation failed OPEN")
	}
	if got := rt.Snapshot().Dispatches; got != before {
		t.Fatalf("dispatch happened on error path: %d -> %d", before, got)
	}
}

// TestB14cCancelRacesAdmission drives the REAL cancel-races-admission
// interleaving (B3): Submit is blocked inside the native admission gate; a
// concurrent cancel records intent (no run exists) and the record stays
// dispatching; Submit returns cancelled_before_admission; the shared
// finishAdmissionLocked must create the missing Cancel metadata with a
// CONFIRMED disposition — never a nil-deref, never cancelled with an empty
// disposition. The cancel handler saw a non-terminal record at its own
// decision point, so the fake emits the native cancelled event exactly as
// the codex attachment does.
func TestB14cCancelRacesAdmission(t *testing.T) {
	ep, rt, store, now := b14cEndpoint(t)

	rt.HoldAdmission()
	id := "11111111-1111-4111-8111-1111111111c4"
	submitDone := make(chan error, 1)
	go func() { _, err := ep.Handle(submitCmd(id), core.Source{Host: "local"}); submitDone <- err }()
	if !b14cWait(func() bool {
		key := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
		rec, found, err := store.Get(key)
		return err == nil && found && rec.State == protocol.StateDispatching
	}) {
		t.Fatal("submit never reached dispatching")
	}

	// Cancel while the record is dispatching and Submit is blocked in the
	// gate. The fake has no run for the key: it records cancelIntent and —
	// because there is no run to abort — emits nothing yet; the intent is
	// consumed by Submit when the gate releases, returning
	// cancelled_before_admission. This is the exact codex shape.
	ref := protocol.EncodeRef("local", "fake", id)
	if _, err := ep.Handle(&protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpRequestCancel, RequestRef: ref,
		TargetID: "fake", Epoch: "e_1", NotAfter: protocol.FormatTime(now().Add(2 * time.Minute)),
	}, core.Source{Host: "local"}); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	rt.ReleaseAdmission()
	if err := <-submitDone; err != nil {
		t.Fatalf("submit: %v", err)
	}

	key := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
	rec, found, err := store.Get(key)
	if err != nil || !found {
		t.Fatalf("record after raced cancel: ok=%v err=%v", found, err)
	}
	if rec.State != protocol.StateCancelled {
		t.Fatalf("state = %s, want cancelled", rec.State)
	}
	if rec.Cancel == nil {
		t.Fatal("cancelled record has NO cancel metadata — B3 nil-deref regression")
	}
	if rec.Cancel.Disposition != protocol.CancelConfirmed {
		t.Fatalf("disposition = %q, want cancelled (confirmed)", rec.Cancel.Disposition)
	}
	if got := rt.Snapshot().Dispatches; got != 1 {
		t.Fatalf("dispatches = %d, want 1 (blocked submit, no double dispatch)", got)
	}
	if rt.Snapshot().Aborts != 0 {
		t.Fatalf("aborts = %d, want 0 (admission never happened)", rt.Snapshot().Aborts)
	}
}

// TestB14cCancelEventDuringAdmission drives the OTHER raced shape: the
// native cancel handler DOES emit EventRunCancelled while Submit is blocked
// (a queued run cancelled natively). onNative sets StateCancelled without
// cancel metadata (none exists); Submit then returns with an admission the
// record no longer reflects. finishAdmissionLocked's non-dispatching branch
// must create the metadata, confirm the disposition, and return the durable
// cancelled snapshot — never an empty success.
func TestB14cCancelEventDuringAdmission(t *testing.T) {
	ep, rt, store, _ := b14cEndpoint(t)

	rt.HoldAdmission()
	id := "11111111-1111-4111-8111-1111111111c5"
	var submitRep protocol.Reply
	submitDone := make(chan error, 1)
	go func() {
		any, err := ep.Handle(submitCmd(id), core.Source{Host: "local"})
		if any != nil {
			submitRep, _ = any.(protocol.Reply)
		}
		submitDone <- err
	}()
	if !b14cWait(func() bool {
		key := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
		rec, found, err := store.Get(key)
		return err == nil && found && rec.State == protocol.StateDispatching
	}) {
		t.Fatal("submit never reached dispatching")
	}

	// The native side cancels the run mid-admission and emits the event,
	// exactly as a queued-run deletion does in the codex attachment.
	rt.CancelRun(id)

	if !b14cWait(func() bool {
		key := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
		rec, found, err := store.Get(key)
		return err == nil && found && rec.State == protocol.StateCancelled
	}) {
		t.Fatal("native cancel event never moved the record to cancelled")
	}

	rt.ReleaseAdmission()
	if err := <-submitDone; err != nil {
		t.Fatalf("submit: %v", err)
	}

	key := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
	rec, found, err := store.Get(key)
	if err != nil || !found {
		t.Fatalf("record after native cancel: ok=%v err=%v", found, err)
	}
	if rec.State != protocol.StateCancelled {
		t.Fatalf("state = %s, want cancelled", rec.State)
	}
	if rec.Cancel == nil || rec.Cancel.Disposition != protocol.CancelConfirmed {
		t.Fatalf("cancel metadata = %+v, want confirmed disposition (B3)", rec.Cancel)
	}
	if submitRep.Snapshot.RequestID == "" || submitRep.Snapshot.State != protocol.StateCancelled {
		t.Fatalf("submit reply snapshot = %+v, want durable cancelled snapshot", submitRep.Snapshot)
	}
	if submitRep.Outcome.Code != protocol.CodeCancelledBeforeAdmission {
		t.Fatalf("submit outcome code = %q, want cancelled_before_admission", submitRep.Outcome.Code)
	}
	if submitRep.Outcome.Disposition != protocol.CancelConfirmed {
		t.Fatalf("submit outcome disposition = %q, want cancelled", submitRep.Outcome.Disposition)
	}
}

// TestB14cAdmitDeferredRacedCancel drives the same B3 shape through the
// deferred admission path: a received record whose target registers later,
// with a cancel landing between the Tick's dispatching commit and the
// native admission returning.
func TestB14cAdmitDeferredRacedCancel(t *testing.T) {
	store, now := openStore(t)
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: store, Now: now})
	t.Cleanup(func() { _ = ep.Close() })

	// Target registered but OFFLINE: the submit stays received (deferred) —
	// a submit to a target that was never shared is refused unshared, not
	// queued, so the offline shape is the only way to reach admitDeferred.
	rt.SetOffline(true)
	ep.Register(rt)
	id := "11111111-1111-4111-8111-1111111111c6"
	if _, err := ep.Handle(submitCmd(id), core.Source{Host: "local"}); err != nil {
		t.Fatalf("deferred submit: %v", err)
	}
	key := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
	if rec, found, err := store.Get(key); err != nil || !found || rec.State != protocol.StateReceived {
		t.Fatalf("offline submit did not defer: state=%s found=%v err=%v", rec.State, found, err)
	}

	// Target comes online with admission held; Tick admits the deferred
	// record into the held gate.
	rt.SetOffline(false)
	rt.HoldAdmission()
	ep.Register(rt)
	tickDone := make(chan error, 1)
	go func() { tickDone <- ep.Tick() }()
	if !b14cWait(func() bool {
		rec, found, err := store.Get(key)
		return err == nil && found && rec.State == protocol.StateDispatching
	}) {
		t.Fatal("deferred record never reached dispatching")
	}

	// Cancel the queued run natively while admission is held.
	rt.CancelRun(id)
	if !b14cWait(func() bool {
		rec, found, err := store.Get(key)
		return err == nil && found && rec.State == protocol.StateCancelled
	}) {
		t.Fatal("native cancel event never moved the deferred record")
	}

	rt.ReleaseAdmission()
	if err := <-tickDone; err != nil {
		t.Fatalf("tick: %v", err)
	}
	if !b14cWait(func() bool {
		rec, found, err := store.Get(key)
		if err != nil || !found || rec.Cancel == nil {
			return false
		}
		return rec.Cancel.Disposition == protocol.CancelConfirmed
	}) {
		rec, _, _ := store.Get(key)
		t.Fatalf("deferred raced cancel never confirmed metadata: %+v", rec)
	}
	rec, _, err := store.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != protocol.StateCancelled {
		t.Fatalf("state = %s, want cancelled", rec.State)
	}
	_ = errors.New // keep errors imported for future probes
}
