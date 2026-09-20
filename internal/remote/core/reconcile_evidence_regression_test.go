package core_test

import (
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// newFakeTarget builds the fake runtime attachment the delegating wrapper
// forwards to.
func newFakeTarget(t *testing.T, store *requests.Store, now func() time.Time) core.Attachment {
	t.Helper()
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: store, Now: now})
	ep.Register(rt)
	return rt
}

// delegatingAttachment forwards every call to a wrapped attachment and
// optionally rewrites the Lookup evidence — the seam a reconcile-time
// regression needs without a full native runtime.
type delegatingAttachment struct {
	inner   core.Attachment
	lookupE *core.Evidence
}

func (d *delegatingAttachment) Inspect() protocol.Session { return d.inner.Inspect() }
func (d *delegatingAttachment) Submit(req core.BoundRequest) (core.Admission, error) {
	return d.inner.Submit(req)
}
func (d *delegatingAttachment) Lookup(key requests.Key, epoch string) (core.Evidence, error) {
	ev, err := d.inner.Lookup(key, epoch)
	if d.lookupE != nil {
		return *d.lookupE, err
	}
	return ev, err
}
func (d *delegatingAttachment) CancelExact(key requests.Key, epoch string) (core.CancelEvidence, error) {
	return d.inner.CancelExact(key, epoch)
}
func (d *delegatingAttachment) Respond(key requests.Key, epoch, interactionID, option string) (protocol.Code, error) {
	return d.inner.Respond(key, epoch, interactionID, option)
}
func (d *delegatingAttachment) AcknowledgeResult(key requests.Key, epoch, digest string) {
	d.inner.AcknowledgeResult(key, epoch, digest)
}
func (d *delegatingAttachment) Subscribe(func(core.NativeEvent)) (unsubscribe func()) {
	return d.inner.Subscribe(nil)
}

// TestTentativeEvidenceIsReconcileNoop pins the P0 regression from review
// 816-r2 (P0-1): reconcileLive must treat EvidenceTentative as "bound but
// native ownership not yet proven" and re-check next tick WITHOUT touching
// the durable record. A duplicated switch case once made the early return
// unreachable, so every reconcile tick committed the record — Revision++
// plus a store write with no state change — on the codex tentative path.
func TestTentativeEvidenceIsReconcileNoop(t *testing.T) {
	store, now := openStore(t)
	rt := &delegatingAttachment{inner: newFakeTarget(t, store, now)}
	ep := core.New(core.Config{Store: store, Now: func() time.Time { return now() }})
	ep.Register(rt)

	id := "11111111-1111-4111-8111-111111111101"
	if _, err := ep.Handle(submitCmd(id), core.Source{Host: "local"}); err != nil {
		t.Fatalf("seed submit: %v", err)
	}
	key := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
	before, ok, err := store.Get(key)
	if err != nil || !ok {
		t.Fatalf("get record: %v (ok=%v)", err, ok)
	}
	if before.State != protocol.StateDispatching && before.State != protocol.StateUncertain {
		// The fake admits, so the record is dispatching/running; force the
		// reconciler to see tentative evidence.
		rt.lookupE = &core.Evidence{Known: true, Class: core.EvidenceTentative, RunID: "e_1"}
	}
	for i := 0; i < 3; i++ {
		if err := ep.Reconcile(); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	after, ok, err := store.Get(key)
	if err != nil || !ok {
		t.Fatalf("get record after reconcile: %v (ok=%v)", err, ok)
	}
	if after.Revision != before.Revision {
		t.Fatalf("tentative evidence committed the record: revision %d -> %d over 3 ticks", before.Revision, after.Revision)
	}
	if after.State != before.State {
		t.Fatalf("tentative evidence moved state %s -> %s", before.State, after.State)
	}
}

// TestRefusalOverProvenAdmissionEndsRejectedTyped pins the A3 mapping (amit-
// remote contract §6): a DEFINITIVE native refusal over PROVEN admission —
// Evidence with RefusalCode set, Admitted true, State rejected — must end the
// record as rejected with the typed refusal code (action-required), never a
// silent dispatch and never evidence of non-admission.
func TestRefusalOverProvenAdmissionEndsRejectedTyped(t *testing.T) {
	store, now := openStore(t)
	rt := &delegatingAttachment{inner: newFakeTarget(t, store, now)}
	ep := core.New(core.Config{Store: store, Now: func() time.Time { return now() }})
	ep.Register(rt)

	id := "11111111-1111-4111-8111-111111111102"
	if _, err := ep.Handle(submitCmd(id), core.Source{Host: "local"}); err != nil {
		t.Fatalf("seed submit: %v", err)
	}
	key := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
	// Fire-time window expiry with a receipt present: admission proven,
	// execution refused, typed code expired (the receipt stays).
	rt.lookupE = &core.Evidence{
		Known: true, Class: core.EvidenceHistoryTerminated, Admitted: true,
		RunID: "e_1", State: protocol.StateRejected, RefusalCode: protocol.CodeExpired,
	}
	if err := ep.Reconcile(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	rec, ok, err := store.Get(key)
	if err != nil || !ok {
		t.Fatalf("get record: %v (ok=%v)", err, ok)
	}
	if rec.State != protocol.StateRejected {
		t.Fatalf("state = %s, want rejected", rec.State)
	}
	if rec.Code != protocol.CodeExpired {
		t.Fatalf("code = %q, want expired", rec.Code)
	}
}
