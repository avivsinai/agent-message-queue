package core_test

import (
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// TestMinEvidenceFloor is the Wave A.4 behavior test. A caller that states
// min_evidence=admitted is REFUSED by an adapter whose strongest submit
// evidence is weaker (refused, not substituted — ADR invariant 5), before any
// side effect: no record, no dispatch. An adapter that publishes no evidence
// projection is class "" and refused too (a `&& s.Evidence != nil`
// guard once inverted the check). An omitted floor keeps
// legacy semantics. Corpus Q01 covers a floor the adapter meets.
func TestMinEvidenceFloor(t *testing.T) {
	weak := &protocol.Evidence{Submit: protocol.EvidenceSubmitted, Completion: "run_terminal"}
	for _, tc := range []struct {
		name     string
		evidence *protocol.Evidence
		floor    string
		refused  bool
	}{
		{"weaker adapter refused", weak, protocol.EvidenceAdmitted, true},
		{"nil projection refused", nil, protocol.EvidenceAdmitted, true},
		{"omitted floor is legacy", weak, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, now := openStore(t)
			ep := core.New(core.Config{Store: store, Now: now})
			t.Cleanup(func() { _ = ep.Close() })
			ep.Register(fake.New("fake", "e_1").WithEvidence(tc.evidence))

			id := "11111111-1111-4111-8111-1111111115a4"
			repAny, err := ep.Handle(&protocol.Command{
				Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit,
				RequestID: id, TargetID: "fake", Epoch: "e_1",
				NotAfter: protocol.FormatTime(now().Add(2 * time.Minute)),
				Input:    &protocol.SubmitInput{Text: "do the thing", MinEvidence: tc.floor},
			}, core.Source{Host: "local"})
			if err != nil {
				t.Fatalf("submit returned error (refusal is in Outcome.Code, not err): %v", err)
			}
			rep := repAny.(protocol.Reply)
			_, exists, _ := store.Get(requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id})
			if !tc.refused {
				if rep.Outcome.Code != "" || rep.Snapshot.State != protocol.StateRunning || !exists {
					t.Fatalf("code=%s state=%s record=%v, want admitted and running", rep.Outcome.Code, rep.Snapshot.State, exists)
				}
				return
			}
			if rep.Outcome.Code != protocol.CodeUnsupported || rep.Snapshot.State != protocol.StateRejected {
				t.Fatalf("code=%s state=%s, want unsupported/rejected", rep.Outcome.Code, rep.Snapshot.State)
			}
			if exists {
				t.Fatal("record was created despite the evidence refusal")
			}
		})
	}
}

// TestMinEvidenceNilInputDoesNotWedgeReconcile is a regression test (#803): admitDeferred
// dereferenced rec.Input.MinEvidence unconditionally, but Record.Input is a
// pointer with omitempty — a state=received record whose on-disk JSON lacks
// input decodes to nil. Reconcile panicked holding e.mu, the deferred
// releaseInFlight re-locked during unwind, and the process wedged forever.
// Now admitDeferred is
// nil-safe and Reconcile returns nil.
func TestMinEvidenceNilInputDoesNotWedgeReconcile(t *testing.T) {
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
