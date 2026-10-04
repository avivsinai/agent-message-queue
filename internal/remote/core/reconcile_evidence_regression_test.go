package core_test

import (
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

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
func (d *delegatingAttachment) Subscribe(fn func(core.NativeEvent)) (unsubscribe func()) {
	return d.inner.Subscribe(fn)
}

// TestTentativeEvidenceIsReconcileNoop pins the P0 regression from review
// 816-r2 (P0-1): reconcileLive must treat EvidenceTentative as "bound but
// native ownership not yet proven" and re-check next tick WITHOUT touching
// the durable record. A duplicated switch case once made the early return
// unreachable, so every reconcile tick committed the record — Revision++
// plus a store write with no state change — on the codex tentative path.
func TestTentativeEvidenceIsReconcileNoop(t *testing.T) {
	store, now := openStore(t)
	rt := &delegatingAttachment{inner: fake.New("fake", "e_1")}
	ep := core.New(core.Config{Store: store, Now: now})
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
	rt.lookupE = &core.Evidence{Known: true, Class: core.EvidenceTentative, RunID: "e_1"}
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

// TestNativeRefusalKeepsTypedCode pins the refusal mapping (pi-bridge
// protocol: adapter recovery and evidence). A definitive native refusal ends
// the record rejected with its typed code, never native_error:
//   - over proven admission (a fire-time window expiry with a receipt);
//   - before delivery: a field defect, pi's refused(generation) after a
//     restart has no receipt, so the evidence is not admitted, and the record
//     ended rejected+native_error instead of keeping stale_epoch.
func TestNativeRefusalKeepsTypedCode(t *testing.T) {
	for _, tc := range []struct {
		name     string
		evidence core.Evidence
		want     protocol.Code
	}{
		{"over proven admission", core.Evidence{
			Known: true, Class: core.EvidenceHistoryTerminated, Admitted: true,
			RunID: "e_1", State: protocol.StateRejected, RefusalCode: protocol.CodeExpired,
		}, protocol.CodeExpired},
		{"before delivery", core.Evidence{
			Known: true, Class: core.EvidenceHistoryTerminated, Admitted: false,
			State: protocol.StateRejected, RefusalCode: protocol.CodeStaleEpoch,
		}, protocol.CodeStaleEpoch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, now := openStore(t)
			rt := &delegatingAttachment{inner: fake.New("fake", "e_1")}
			ep := core.New(core.Config{Store: store, Now: now})
			ep.Register(rt)

			id := "11111111-1111-4111-8111-111111111103"
			if _, err := ep.Handle(submitCmd(id), core.Source{Host: "local"}); err != nil {
				t.Fatalf("seed submit: %v", err)
			}
			ev := tc.evidence
			rt.lookupE = &ev
			if err := ep.Reconcile(); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			rec, ok, err := store.Get(requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id})
			if err != nil || !ok {
				t.Fatalf("get record: %v (ok=%v)", err, ok)
			}
			if rec.State != protocol.StateRejected || rec.Code != tc.want {
				t.Fatalf("record = %s/%q, want rejected/%s", rec.State, rec.Code, tc.want)
			}
		})
	}
}
