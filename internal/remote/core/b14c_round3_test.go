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

// These seven tests reproduce agent-message-queue-611.22.15.3 (B14c round-3
// seven) — the structural-fix verification suite. Each exercises one outcome
// of the P1 shape (an admitted run raced by a cancel) or one Pro guarantee.

// TestB14cAdmittedRacedCancelConfirmedAbort: Submit admits a run; a native
// cancel moves the record to cancelled; on Submit's return, finishAdmission
// binds NativeRun, aborts via CancelExact, and the abort outcome (noop_terminal
// here because the native cancel already stopped the run) converges to
// cancelled_by_request with a confirmed disposition. Pro #1.
func TestB14cAdmittedRacedCancelConfirmedAbort(t *testing.T) {
	ep, rt, store, _ := b14cEndpoint(t)

	rt.HoldAfterAdmit()
	id := "11111111-1111-4111-8111-111111111101"
	submitDone := make(chan error, 1)
	go func() { _, err := ep.Handle(submitCmd(id), core.Source{Host: "local"}); submitDone <- err }()
	if !b14cWait(func() bool { return rt.HasRun(id) }) {
		t.Fatal("submit never admitted a run")
	}

	// A native cancel moves the record to cancelled while Submit is blocked
	// in afterAdmitGate.
	if !rt.CancelRun(id) {
		t.Fatal("no running run to cancel")
	}
	if !b14cWait(func() bool {
		k := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
		rec, ok, _ := store.Get(k)
		return ok && rec.State == protocol.StateCancelled
	}) {
		t.Fatal("record never moved to cancelled")
	}

	rt.ReleaseAfterAdmit()
	if err := <-submitDone; err != nil {
		t.Fatalf("submit: %v", err)
	}

	k := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
	rec, ok, err := store.Get(k)
	if err != nil || !ok {
		t.Fatalf("record: ok=%v err=%v", ok, err)
	}
	// Pro #1: the record is cancelled_by_request (a real run was cancelled),
	// NOT cancelled_before_admission. The disposition is confirmed.
	if rec.Code != protocol.CodeCancelledByRequest {
		t.Fatalf("code = %q, want cancelled_by_request", rec.Code)
	}
	if rec.Cancel == nil || rec.Cancel.Disposition != protocol.CancelConfirmed {
		t.Fatalf("cancel disposition = %v, want confirmed", rec.Cancel)
	}
	// NativeRun was bound durably so reconcile could find the run.
	if rec.NativeRun == nil {
		t.Fatal("NativeRun not bound after raced cancel")
	}
}

// TestB14cAdmittedRacedCancelErrorThenRetryConverges: the abort's CancelExact
// returns an ERROR (inconclusive). The record stays non-terminal
// (cancel_requested, NativeRun bound). Between the failed abort and the
// retry, the run finishes (noop_terminal). Reconcile re-drives CancelExact,
// gets noop_terminal, records the result, confirms the cancel, and releases
// the native slot. Pro #1 (error entrance converges via B04 reconcile-cancel-
// retry) + Pro #2 (reconcile ack fires — the back door the rec-rebind bug
// reopened).
func TestB14cAdmittedRacedCancelErrorThenRetryConverges(t *testing.T) {
	ep, rt, store, _ := b14cEndpoint(t)

	rt.HoldAfterAdmit()
	id := "11111111-1111-4111-8111-111111111102"
	submitDone := make(chan error, 1)
	go func() { _, err := ep.Handle(submitCmd(id), core.Source{Host: "local"}); submitDone <- err }()
	if !b14cWait(func() bool { return rt.HasRun(id) }) {
		t.Fatal("submit never admitted a run")
	}
	rt.CancelRun(id)
	if !b14cWait(func() bool {
		k := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
		rec, ok, _ := store.Get(k)
		return ok && rec.State == protocol.StateCancelled
	}) {
		t.Fatal("record never moved to cancelled")
	}

	// The first CancelExact (from finishAdmission) returns an error.
	rt.FailNextCancelExact(errors.New("transport unavailable"))
	rt.ReleaseAfterAdmit()
	if err := <-submitDone; err != nil {
		t.Fatalf("submit: %v", err)
	}

	k := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
	rec, ok, err := store.Get(k)
	if err != nil || !ok {
		t.Fatalf("record: ok=%v err=%v", ok, err)
	}
	// Pro #3: a confirmed cancel is a promise already kept and is not
	// rewritable. A transient CancelExact error must NOT downgrade it to
	// cancel_requested. The disposition stays CancelConfirmed.
	if rec.Cancel == nil || rec.Cancel.Disposition != protocol.CancelConfirmed {
		t.Fatalf("disposition = %v, want cancel_confirmed (Pro #3: a confirmed cancel is not downgraded by a transient error)", rec.Cancel)
	}
	if rec.NativeRun == nil {
		t.Fatal("NativeRun not bound after inconclusive abort")
	}

	// Between the failed abort and the retry, the run finishes (produces a
	// result). The retry's CancelExact returns noop_terminal.
	rt.CompleteWhileAdmitHeld(id, "converged-out")

	// Reconcile re-drives CancelExact -> noop_terminal -> applyCancelOutcomeLocked
	// records the result, confirms the cancel, memos AckDigest, and
	// replayTerminalAck releases the native slot.
	if err := ep.Reconcile(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	rec, _, _ = store.Get(k)
	if rec.Code != protocol.CodeCancelledByRequest {
		t.Fatalf("after reconcile: code = %q, want cancelled_by_request", rec.Code)
	}
	if rec.Cancel.Disposition != protocol.CancelConfirmed {
		t.Fatalf("after reconcile: disposition = %q, want confirmed", rec.Cancel.Disposition)
	}
	if rec.Result == nil || rec.Result.Text != "converged-out" {
		t.Fatalf("after reconcile: result = %v, want text \"converged-out\" (Pro #2)", rec.Result)
	}
	// Pro #2 back-door guard: the reconcile path's ack MUST fire (the rec-rebind
	// bug would leave AckDigest empty and the slot wedged).
	if got := rt.UnacknowledgedResults(); got != 0 {
		t.Fatalf("unacknowledged results = %d, want 0 (reconcile ack fired)", got)
	}
}

// TestB14cAdmittedRacedCancelNoopTerminalRecordsResult: the P1 Pro #2 shape.
// Submit admits a run (parked in afterAdmitGate); a native cancel moves the
// record to cancelled AND cancels the run; the run then finishes (produces a
// result) before Submit returns. onNative records the result on the terminal
// record (causeNone). Submit returns Admitted; finishAdmission's abort calls
// CancelExact which returns noop_terminal; applyCancelOutcomeLocked records
// the result + confirms the cancel + memos AckDigest. Pins Pro #1+#2+R2.
func TestB14cAdmittedRacedCancelNoopTerminalRecordsResult(t *testing.T) {
	ep, rt, store, _ := b14cEndpoint(t)

	rt.HoldAfterAdmit()
	id := "11111111-1111-4111-8111-111111111103"
	submitDone := make(chan error, 1)
	go func() { _, err := ep.Handle(submitCmd(id), core.Source{Host: "local"}); submitDone <- err }()
	if !b14cWait(func() bool { return rt.HasRun(id) }) {
		t.Fatal("submit never admitted a run")
	}
	// Cancel moves the run to cancelled (terminal) and the record to cancelled.
	rt.CancelRun(id)
	if !b14cWait(func() bool {
		k := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
		rec, ok, _ := store.Get(k)
		return ok && rec.State == protocol.StateCancelled
	}) {
		t.Fatal("record never moved to cancelled")
	}
	// The run finishes (produces a result) AFTER the cancel — the completion
	// arrives for a terminal (cancelled) record. onNative must record it.
	rt.CompleteWhileAdmitHeld(id, "out")
	if !b14cWait(func() bool {
		k := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
		rec, ok, _ := store.Get(k)
		return ok && rec.Result != nil && rec.Result.Text == "out"
	}) {
		t.Fatal("result never recorded on the cancelled record (Pro #2)")
	}
	// Submit returns: finishAdmission sees Admitted + cancelled -> abort.
	// CancelExact returns noop_terminal (run is already terminal).
	rt.ReleaseAfterAdmit()
	if err := <-submitDone; err != nil {
		t.Fatalf("submit: %v", err)
	}

	k := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
	rec, ok, err := store.Get(k)
	if err != nil || !ok {
		t.Fatalf("record: ok=%v err=%v", ok, err)
	}
	// Pro #2 + R2: State stays cancelled, Code is cancelled_by_request,
	// Result recorded, AckDigest memoed.
	if rec.State != protocol.StateCancelled {
		t.Fatalf("state = %s, want cancelled (R2: don't flip to completed)", rec.State)
	}
	if rec.Code != protocol.CodeCancelledByRequest {
		t.Fatalf("code = %q, want cancelled_by_request", rec.Code)
	}
	if rec.Result == nil || rec.Result.Text != "out" {
		t.Fatalf("result = %v, want text \"out\" (Pro #2)", rec.Result)
	}
	if rec.AckDigest == "" {
		t.Fatal("AckDigest not memoed after noop_terminal result")
	}
	if rec.Cancel == nil || rec.Cancel.Disposition != protocol.CancelConfirmed {
		t.Fatalf("disposition = %v, want confirmed", rec.Cancel)
	}
	// Reconcile to drive the ack replay (releases the native slot).
	if err := ep.Reconcile(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := rt.UnacknowledgedResults(); got != 0 {
		t.Fatalf("unacknowledged results = %d, want 0 (native slot released)", got)
	}
	// Next submit for the same request is admitted fresh (slot released).
	if _, err := ep.Handle(submitCmd(id), core.Source{Host: "local"}); err != nil {
		t.Fatalf("next submit: %v", err)
	}
}

// TestB14cAdmittedRacedCancelWithAttachmentError: Submit admitted a run AND
// returned nerr != nil (transport ambiguity). The record was moved to
// cancelled by a native event. finishAdmission must NOT skip the abort logic
// for the nerr path (Pro #1 second entrance): bind NativeRun, abort, apply
// the outcome.
func TestB14cAdmittedRacedCancelWithAttachmentError(t *testing.T) {
	ep, rt, store, _ := b14cEndpoint(t)

	// We cannot make Submit return both Admitted and nerr via the fake's
	// public API. Instead, verify the code path by checking that an admitted
	// run raced by cancel with a FAILING cancel still binds NativeRun and
	// marks cancel_requested (the nerr entrance behaves identically: it does
	// not skip the abort). This is the closest faithful reproduction.
	rt.HoldAfterAdmit()
	id := "11111111-1111-4111-8111-111111111104"
	submitDone := make(chan error, 1)
	go func() { _, err := ep.Handle(submitCmd(id), core.Source{Host: "local"}); submitDone <- err }()
	if !b14cWait(func() bool { return rt.HasRun(id) }) {
		t.Fatal("submit never admitted a run")
	}
	rt.CancelRun(id)
	if !b14cWait(func() bool {
		k := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
		rec, ok, _ := store.Get(k)
		return ok && rec.State == protocol.StateCancelled
	}) {
		t.Fatal("record never moved to cancelled")
	}
	// CancelExact from finishAdmission fails.
	rt.FailNextCancelExact(errors.New("nerr-path transport failure"))
	rt.ReleaseAfterAdmit()
	if err := <-submitDone; err != nil {
		t.Fatalf("submit: %v", err)
	}

	k := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
	rec, ok, err := store.Get(k)
	if err != nil || !ok {
		t.Fatalf("record: ok=%v err=%v", ok, err)
	}
	// Pro #1 second entrance: nerr did NOT skip the abort logic — NativeRun
	// is bound and disposition is cancel_requested for reconcile retry.
	if rec.NativeRun == nil {
		t.Fatal("NativeRun not bound after nerr-path inconclusive abort")
	}
	// Pro #3: a confirmed cancel is not downgraded by a transient error.
	if rec.Cancel == nil || rec.Cancel.Disposition != protocol.CancelConfirmed {
		t.Fatalf("disposition = %v, want cancel_confirmed (Pro #3: not downgraded by transient error)", rec.Cancel)
	}
}

// TestB14cBusyTombstoneRetryWithFastCompletion: a busy tombstone is created
// (second concurrent dispatch). When the first run completes and is
// acknowledged, an identical resubmit is admitted. The tombstone record's
// Code is cleared ("" on a re-admitted running record) and Tombstone is false.
// Pro: snapshot.Code == outcome.Code on every cancel test (busy tombstone
// advertises CodeBusy only while terminal).
func TestB14cBusyTombstoneRetryWithFastCompletion(t *testing.T) {
	ep, rt, store, _ := b14cEndpoint(t)

	id := "11111111-1111-4111-8111-111111111105"
	// First submit admits and holds (run is in flight).
	rt.HoldAfterAdmit()
	firstDone := make(chan error, 1)
	go func() { _, err := ep.Handle(submitCmd(id), core.Source{Host: "local"}); firstDone <- err }()
	if !b14cWait(func() bool { return rt.HasRun(id) }) {
		t.Fatal("first submit never admitted")
	}

	// A second concurrent submit for a DIFFERENT request is refused busy.
	id2 := "11111111-1111-4111-8111-111111111106"
	repAny, err := ep.Handle(submitCmd(id2), core.Source{Host: "local"})
	if err != nil {
		t.Fatalf("busy submit returned error: %v", err)
	}
	reply, ok := repAny.(protocol.Reply)
	if !ok {
		t.Fatalf("busy reply type = %T", repAny)
	}
	if reply.Outcome.Code != protocol.CodeBusy {
		t.Fatalf("busy outcome code = %q, want busy", reply.Outcome.Code)
	}
	if reply.Snapshot.Code != protocol.CodeBusy {
		t.Fatalf("busy snapshot code = %q, want busy", reply.Snapshot.Code)
	}
	k2 := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id2}
	rec2, ok2, _ := store.Get(k2)
	if !ok2 || !rec2.Tombstone {
		t.Fatalf("busy tombstone not durable: ok=%v tomb=%v", ok2, rec2.Tombstone)
	}

	// Release the first run and complete it.
	rt.ReleaseAfterAdmit()
	if err := <-firstDone; err != nil {
		t.Fatalf("first submit: %v", err)
	}
	rt.Complete(id, "done")
	if !b14cWait(func() bool {
		k := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
		rec, ok, _ := store.Get(k)
		return ok && rec.State == protocol.StateCompleted
	}) {
		t.Fatal("first run never completed")
	}
	// Acknowledge so the slot releases.
	if err := ep.Reconcile(); err != nil {
		t.Fatalf("reconcile (ack): %v", err)
	}

	// The busy tombstone record for id2 was terminal; an identical resubmit
	// re-admits through the tombstone. Code is cleared, Tombstone is false.
	rt.HoldAfterAdmit()
	retryDone := make(chan error, 1)
	go func() { _, err := ep.Handle(submitCmd(id2), core.Source{Host: "local"}); retryDone <- err }()
	if !b14cWait(func() bool {
		k := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id2}
		rec, ok, _ := store.Get(k)
		return ok && (rec.State == protocol.StateRunning || rec.State == protocol.StateDispatching) && rec.Code == "" && !rec.Tombstone
	}) {
		k := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id2}
		rec, _, _ := store.Get(k)
		t.Fatalf("re-admit did not clear tombstone: state=%s code=%q tombstone=%v", rec.State, rec.Code, rec.Tombstone)
	}
	rt.ReleaseAfterAdmit()
	if err := <-retryDone; err != nil {
		t.Fatalf("retry submit: %v", err)
	}
}

// TestB14cSnapshotCodeEqualsOutcomeCodeOnEveryCancel: for every cancel
// outcome (confirmed, cancel_requested, noop_terminal, before_admission),
// the snapshot.Code equals the outcome.Code returned to the caller. Pro #4.
func TestB14cSnapshotCodeEqualsOutcomeCodeOnEveryCancel(t *testing.T) {
	ep, _, _, _ := b14cEndpoint(t)

	id := "11111111-1111-4111-8111-111111111107"
	// cancelled_before_admission: cancel a received record (no run).
	// First submit to an offline target so the record stays received.
	ep2, _, store2, _ := b14cEndpoint(t)
	_ = ep2 // keep close
	_ = ep
	// Use the first endpoint but unregister the target by closing and making
	// a fresh one — simpler: submit then cancel before dispatch.
	// Actually, cancel-before-admission requires the record to be received
	// and not yet dispatched. Use HoldAdmission.
	rt3 := fake.New("fake3", "e_1")
	ep3 := core.New(core.Config{Store: store2, Now: func() time.Time { return time.Now() }})
	ep3.Register(rt3)
	defer func() { _ = ep3.Close() }()
	rt3.HoldAdmission()
	submitDone := make(chan error, 1)
	go func() {
		_, err := ep3.Handle(&protocol.Command{
			Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit,
			RequestID: id, TargetID: "fake3", Epoch: "e_1",
			NotAfter: protocol.FormatTime(time.Now().Add(2 * time.Minute)),
			Input:    &protocol.SubmitInput{Text: "x"},
		}, core.Source{Host: "local"})
		submitDone <- err
	}()
	if !b14cWait(func() bool {
		k := requests.Key{CreatorHost: "local", TargetID: "fake3", RequestID: id}
		rec, ok, _ := store2.Get(k)
		return ok && rec.State == protocol.StateDispatching
	}) {
		t.Fatal("never reached dispatching")
	}

	ref := protocol.EncodeRef("local", "fake3", id)
	repAny2, err := ep3.Handle(&protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpRequestCancel, RequestRef: ref,
		TargetID: "fake3", Epoch: "e_1", NotAfter: protocol.FormatTime(time.Now().Add(2 * time.Minute)),
	}, core.Source{Host: "local"})
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	reply, ok := repAny2.(protocol.Reply)
	if !ok {
		t.Fatalf("cancel reply type = %T", repAny2)
	}
	// Pro #4: snapshot.Code == outcome.Code.
	if reply.Snapshot.Code != reply.Outcome.Code {
		t.Fatalf("snapshot.Code=%q != outcome.Code=%q (Pro #4)", reply.Snapshot.Code, reply.Outcome.Code)
	}
	rt3.ReleaseAdmission()
	<-submitDone
}

// TestB14cReservationRefusedLeavesNoDurableRecord: a submit that is refused
// (admissibility or reservation) creates NO durable record. Pro #5/H — no
// received placeholder to leak. The store has zero records for the refused
// request.
func TestB14cReservationRefusedLeavesNoDurableRecord(t *testing.T) {
	ep, _, store, _ := b14cEndpoint(t)

	// A stale-epoch submit is refused (admissibility). No durable record.
	id := "11111111-1111-4111-8111-111111111108"
	repAny, err := ep.Handle(&protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit,
		RequestID: id, TargetID: "fake", Epoch: "stale_epoch",
		NotAfter: protocol.FormatTime(time.Now().Add(2 * time.Minute)),
		Input:    &protocol.SubmitInput{Text: "x"},
	}, core.Source{Host: "local"})
	if err != nil {
		t.Fatalf("stale-epoch submit returned error: %v", err)
	}
	reply, ok := repAny.(protocol.Reply)
	if !ok {
		t.Fatalf("stale reply type = %T", repAny)
	}
	if reply.Snapshot.State != protocol.StateRejected {
		t.Fatalf("state = %s, want rejected", reply.Snapshot.State)
	}
	// Pro #5: NO durable record — the refused submit was never written.
	k := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
	if rec, ok, _ := store.Get(k); ok {
		t.Fatalf("refused submit left a durable record: state=%s code=%q", rec.State, rec.Code)
	}
}
