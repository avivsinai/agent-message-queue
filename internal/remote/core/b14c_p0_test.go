package core_test

import (
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// TestP0TickDoesNotCancelHealthyRunningRequest reproduces P0 #1: the
// needsRuntimeSettlement predicate fired on healthy running work (NativeRun !=
// nil && AckDigest == "" is true for a simply-running record) and routed it to
// reconcileCancelRetry, which called CancelExact unconditionally — killing
// every remote prompt within one tick.
//
// FIX: the predicate is split into owesCancel (a cancel we have not confirmed)
// and owesAck (a result we have not released). A healthy running record
// matches neither and is a noop.
//
// Acceptance: submit -> running; one Tick -> still running, CancelExact never
// called. This test would have caught the P0 before Pro ever saw it.
func TestP0TickDoesNotCancelHealthyRunningRequest(t *testing.T) {
	ep, rt, store, _ := b14cEndpoint(t)
	t.Cleanup(func() { _ = ep.Close() })

	id := "11111111-1111-4111-8111-111111111910"
	if _, err := ep.Handle(submitCmd(id), core.Source{Host: "local"}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !b14cWait(func() bool { return rt.HasRun(id) }) {
		t.Fatal("submit never admitted a run")
	}

	// Precondition: the record is running with NativeRun bound and no ack.
	k := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
	rec, ok, _ := store.Get(k)
	if !ok || rec.State != protocol.StateRunning {
		t.Fatalf("precondition: state=%s, want running", recState(rec, ok))
	}
	if rec.NativeRun == nil {
		t.Fatal("precondition: NativeRun not bound")
	}
	if rec.AckDigest != "" {
		t.Fatal("precondition: AckDigest should be empty for a running record")
	}

	cancelCallsBefore := rt.CancelExactCount()

	// ONE Tick. With the broken predicate, this called CancelExact and
	// transitioned to cancelled. With the fix, it is a noop.
	if err := ep.Tick(); err != nil {
		t.Fatalf("tick: %v", err)
	}

	cancelCallsAfter := rt.CancelExactCount()
	if cancelCallsAfter != cancelCallsBefore {
		t.Fatalf("Tick called CancelExact %d time(s) on a healthy running record (Pro P0 #1 — kills every prompt within one tick)", cancelCallsAfter-cancelCallsBefore)
	}

	// The record must still be running.
	rec, _, _ = store.Get(k)
	if rec.State != protocol.StateRunning {
		t.Fatalf("after Tick: state=%s, want running (Pro P0 #1 — Tick cancelled healthy work)", rec.State)
	}
	if rec.NativeRun == nil {
		t.Fatal("after Tick: NativeRun was cleared")
	}
}
