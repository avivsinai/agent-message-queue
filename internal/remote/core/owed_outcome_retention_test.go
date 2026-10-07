package core_test

import (
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
	"sync"
)

// silentResolver wraps an attachment as an InteractionResolver over an
// outcome map the test controls: empty it stays silent (the unsettleable
// seam — a bridge that never wrote interaction_resolved, or a removed or
// rotated one); the test fills it to play the resolvable case. The fake
// runtime itself stays a plain Attachment, so no other test's endpoint
// behavior changes.
type silentResolver struct {
	core.Attachment
	mu  sync.Mutex
	res map[string]protocol.Resolution
}

func (s *silentResolver) ResolvedInteraction(_ requests.Key, _, interactionID string) (protocol.Resolution, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, ok := s.res[interactionID]
	return res, ok
}

func (s *silentResolver) resolve(interactionID string, res protocol.Resolution) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.res[interactionID] = res
}

// Bead agent-message-queue-611.45 (advisor review of #926): a completed
// record that owes an approval outcome the resolver never finds must not
// stay in the store forever. Given a completed record that owes an outcome
// the resolver never finds, when the retention horizon passes, the record
// shows an explicit uncertain resolution and compaction proceeds; a
// resolvable owed outcome still settles normally.
func TestOwedOutcomeRetention(t *testing.T) {
	store, now := openStore(t)
	clk := now()
	rt := fake.New("fake", "e_1")
	att := &silentResolver{Attachment: rt, res: map[string]protocol.Resolution{}}
	ep := core.New(core.Config{Store: store, Now: func() time.Time { return clk }})
	ep.Register(att)

	id := "11111111-1111-4111-8111-111111111145"
	if _, err := ep.Handle(submitCmd(id), ownerShare); err != nil {
		t.Fatal(err)
	}
	// A pending question, then the run completes while it is open: the
	// terminal transition clears the interaction and owes its outcome.
	rt.Question(id, "i_1", []string{"yes", "no"})
	rt.Complete(id, "done")

	key := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
	rec, ok, err := store.Get(key)
	if err != nil || !ok {
		t.Fatalf("record after completion: ok=%v err=%v", ok, err)
	}
	if len(rec.OwedOutcomes) != 1 || rec.OwedOutcomes[0] != "i_1" {
		t.Fatalf("owed outcomes = %v, want [i_1]", rec.OwedOutcomes)
	}

	// Reconcile before the horizon: the obligation survives untouched.
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	rec, _, _ = store.Get(key)
	if len(rec.OwedOutcomes) != 1 {
		t.Fatalf("owed outcomes after an in-horizon reconcile = %v, want the obligation kept", rec.OwedOutcomes)
	}

	// The horizon passes with the resolver still silent.
	clk = clk.Add(protocol.DefaultCompactHorizon + time.Minute)
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	rec, _, _ = store.Get(key)
	if len(rec.OwedOutcomes) != 0 {
		t.Fatalf("owed outcomes after the horizon = %v, want the obligation retired", rec.OwedOutcomes)
	}
	want := protocol.Resolution{InteractionID: "i_1", Outcome: protocol.ResolutionDeliveryUnknown}
	if len(rec.Resolved) == 0 || rec.Resolved[len(rec.Resolved)-1] != want {
		t.Fatalf("resolved = %+v, want an explicit %s resolution for i_1", rec.Resolved, want.Outcome)
	}
	// The record's state never changed: the terminal result stands.
	if rec.State != protocol.StateCompleted || rec.Result == nil || rec.Result.Text != "done" {
		t.Fatalf("retention changed the terminal record: state=%s result=%+v", rec.State, rec.Result)
	}

	// Compaction, which the owed outcome used to block forever, proceeds.
	if ok, err := store.CompactOne(key, clk.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("compact after retention: ok=%v err=%v, want the record compacted", ok, err)
	}
}

// The resolvable case is unchanged: an outcome the resolver reports settles
// normally on Reconcile, inside the horizon, with the exact resolution.
func TestOwedOutcomeStillSettlesNormally(t *testing.T) {
	store, now := openStore(t)
	clk := now()
	rt := fake.New("fake", "e_1")
	att := &silentResolver{Attachment: rt, res: map[string]protocol.Resolution{}}
	ep := core.New(core.Config{Store: store, Now: func() time.Time { return clk }})
	ep.Register(att)

	id := "11111111-1111-4111-8111-111111111146"
	if _, err := ep.Handle(submitCmd(id), ownerShare); err != nil {
		t.Fatal(err)
	}
	rt.Question(id, "i_2", []string{"yes", "no"})
	rt.Complete(id, "done")

	key := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
	rec, ok, err := store.Get(key)
	if err != nil || !ok {
		t.Fatalf("record after completion: ok=%v err=%v", ok, err)
	}
	if len(rec.OwedOutcomes) != 1 || rec.OwedOutcomes[0] != "i_2" {
		t.Fatalf("owed outcomes = %v, want [i_2]", rec.OwedOutcomes)
	}

	// The resolver finds the outcome (the bridge did write it after all).
	att.resolve("i_2", protocol.Resolution{InteractionID: "i_2", Outcome: protocol.ResolutionAnswered, Option: "yes"})
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	rec, _, _ = store.Get(key)
	if len(rec.OwedOutcomes) != 0 {
		t.Fatalf("owed outcomes after a resolving reconcile = %v, want settled", rec.OwedOutcomes)
	}
	want := protocol.Resolution{InteractionID: "i_2", Outcome: protocol.ResolutionAnswered, Option: "yes"}
	if len(rec.Resolved) == 0 || rec.Resolved[len(rec.Resolved)-1] != want {
		t.Fatalf("resolved = %+v, want %+v", rec.Resolved, want)
	}
}
