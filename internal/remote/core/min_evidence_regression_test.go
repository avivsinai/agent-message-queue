package core_test

import (
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// TestMinEvidenceB1NilInputDoesNotWedgeReconcile is the regression test for
// the round-1 review blocker B1 (611.22.19 round-2): admitDeferred
// dereferenced rec.Input.MinEvidence unconditionally, but Record.Input is a
// pointer with omitempty — a state=received record whose on-disk JSON lacks
// input decodes to nil. Reconcile panicked holding e.mu, the deferred
// releaseInFlight re-locked during unwind, and the process wedged forever.
// Main returns nil in 0.04s; the PR head panicked. Now admitDeferred is
// nil-safe and Reconcile returns nil.
func TestMinEvidenceB1NilInputDoesNotWedgeReconcile(t *testing.T) {
	store, now := openStore(t)
	ep := core.New(core.Config{Store: store, Now: now})
	t.Cleanup(func() { _ = ep.Close() })

	rt := fake.New("fake", "e_1")
	ep.Register(rt)

	// Create a received record with NO Input (simulating an older-version
	// or damaged store file where the input JSON was omitted). This is the
	// exact threat the :1889 comment names.
	rec := &requests.Record{
		Snapshot: protocol.Snapshot{
			Schema:      protocol.SchemaRequest,
			RequestID:   "11111111-1111-4111-8111-1111111115b1",
			CreatorHost: "local",
			TargetID:    "fake",
			Epoch:       "e_1",
			Revision:    1,
			State:       protocol.StateReceived,
			NotAfter:    protocol.FormatTime(now().Add(2 * time.Minute)),
		},
		Input: nil, // the bug: this was dereferenced unconditionally
	}
	if err := store.Create(rec); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Reconcile must return nil, not panic. A timeout proves the wedge.
	done := make(chan error, 1)
	go func() { done <- ep.Reconcile() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Reconcile with nil-Input record: want nil, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Reconcile wedged (nil-Input panic holding e.mu)")
	}
}

// TestMinEvidenceB2NilEvidenceProjectionRefused is the regression test for
// the round-1 review blocker B2 (611.22.19 round-2): an attachment that
// publishes NO evidence projection (Session.Evidence == nil) was admitted
// and dispatched under min_evidence=admitted because the conjunct
// `&& s.Evidence != nil` inverted the check. The protocol layer already
// fails closed (EvidenceClassMeets("", admitted) == false); the endpoint
// now matches: a nil projection is class "" and is refused unsupported.
func TestMinEvidenceB2NilEvidenceProjectionRefused(t *testing.T) {
	store, now := openStore(t)
	ep := core.New(core.Config{Store: store, Now: now})
	t.Cleanup(func() { _ = ep.Close() })

	// An adapter that publishes NO evidence projection (nil Evidence).
	rt := fake.New("fake", "e_1").WithEvidence(nil)
	ep.Register(rt)

	id := "11111111-1111-4111-8111-1111111115b2"
	cmd := &protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit,
		RequestID: id, TargetID: "fake", Epoch: "e_1",
		NotAfter: protocol.FormatTime(now().Add(2 * time.Minute)),
		Input:    &protocol.SubmitInput{Text: "do the thing", MinEvidence: protocol.EvidenceAdmitted},
	}
	repAny, err := ep.Handle(cmd, core.Source{Host: "local"})
	if err != nil {
		t.Fatalf("submit returned error (refusal is in Outcome.Code, not err): %v", err)
	}
	rep := repAny.(protocol.Reply)
	if rep.Outcome.Code != protocol.CodeUnsupported {
		t.Fatalf("nil-evidence adapter with min_evidence=admitted: code=%s, want unsupported; state=%s",
			rep.Outcome.Code, rep.Snapshot.State)
	}
	if rep.Snapshot.State != protocol.StateRejected {
		t.Fatalf("state=%s, want rejected", rep.Snapshot.State)
	}
	// No side effect: no record was created.
	_, exists, _ := store.Get(requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id})
	if exists {
		t.Fatal("record was created despite nil-evidence refusal")
	}
}
