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

// The horizon retires even when the target is GONE or its attachment has no
// InteractionResolver (611.45 r2 P2): a record that survives a restart after
// its target disappeared must not stay pinned forever.
func TestOwedOutcomeRetiresWithUnregisteredTarget(t *testing.T) {
	store, now := openStore(t)
	clk := now()
	rt := fake.New("fake", "e_1")
	att := &silentResolver{Attachment: rt, res: map[string]protocol.Resolution{}}
	ep := core.New(core.Config{Store: store, Now: func() time.Time { return clk }})
	ep.Register(att)

	id := "11111111-1111-4111-8111-111111111147"
	if _, err := ep.Handle(submitCmd(id), ownerShare); err != nil {
		t.Fatal(err)
	}
	rt.Question(id, "i_3", []string{"yes", "no"})
	rt.Complete(id, "done")
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}

	// The target disappears entirely: unregistered, no resolver anywhere.
	ep.UnregisterAll()
	key := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
	clk = clk.Add(protocol.DefaultCompactHorizon + time.Minute)
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	rec, _, err := store.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.OwedOutcomes) != 0 || len(rec.RetiredOutcomes) != 1 {
		t.Fatalf("after the horizon with no target: owed=%v retired=%v, want the obligation retired", rec.OwedOutcomes, rec.RetiredOutcomes)
	}
	if ok, err := store.CompactOne(key, clk.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("compact after retirement without a target: ok=%v err=%v, want compacted", ok, err)
	}
}

// A resolution that arrives between recovery's unlocked resolver lookup and
// its locked update is not retired behind its own resolution, and a
// resolution that arrives after retirement still corrects the record: the
// resolver-reported answered outcome replaces the uncertain one (611.45 r2
// P1, resolver-only correction).
func TestOwedOutcomeCorrectableAfterRetirement(t *testing.T) {
	store, now := openStore(t)
	clk := now()
	rt := fake.New("fake", "e_1")
	att := &silentResolver{Attachment: rt, res: map[string]protocol.Resolution{}}
	ep := core.New(core.Config{Store: store, Now: func() time.Time { return clk }})
	ep.Register(att)

	id := "11111111-1111-4111-8111-111111111148"
	if _, err := ep.Handle(submitCmd(id), ownerShare); err != nil {
		t.Fatal(err)
	}
	rt.Question(id, "i_4", []string{"yes", "no"})
	rt.Complete(id, "done")
	key := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}

	// Past the horizon the obligation retires as delivery_unknown.
	clk = clk.Add(protocol.DefaultCompactHorizon + time.Minute)
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	rec, _, err := store.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.RetiredOutcomes) != 1 || rec.RetiredOutcomes[0] != "i_4" {
		t.Fatalf("retired outcomes = %v, want [i_4]", rec.RetiredOutcomes)
	}
	want := protocol.Resolution{InteractionID: "i_4", Outcome: protocol.ResolutionDeliveryUnknown}
	if len(rec.Resolved) == 0 || rec.Resolved[len(rec.Resolved)-1] != want {
		t.Fatalf("resolved = %+v, want the retention resolution", rec.Resolved)
	}

	// The resolver gains the exact outcome afterwards; recovery corrects.
	att.resolve("i_4", protocol.Resolution{InteractionID: "i_4", Outcome: protocol.ResolutionAnswered, Option: "yes"})
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	rec, _, _ = store.Get(key)
	want = protocol.Resolution{InteractionID: "i_4", Outcome: protocol.ResolutionAnswered, Option: "yes"}
	if len(rec.RetiredOutcomes) != 0 || len(rec.Resolved) == 0 || rec.Resolved[len(rec.Resolved)-1] != want {
		t.Fatalf("after a correcting reconcile: retired=%v resolved=%+v, want %+v", rec.RetiredOutcomes, rec.Resolved, want)
	}
}

// A late native EventQuestionResolved after retirement corrects the record
// even though recovery no longer owes the id (611.45 r2 P1, native-event
// correction): the answered outcome replaces the uncertain one.
func TestRetiredOutcomeCorrectedByLateNativeResolution(t *testing.T) {
	store, now := openStore(t)
	clk := now()
	rt := fake.New("fake", "e_1")
	att := &silentResolver{Attachment: rt, res: map[string]protocol.Resolution{}}
	ep := core.New(core.Config{Store: store, Now: func() time.Time { return clk }})
	ep.Register(att)

	id := "11111111-1111-4111-8111-111111111149"
	if _, err := ep.Handle(submitCmd(id), ownerShare); err != nil {
		t.Fatal(err)
	}
	rt.Question(id, "i_5", []string{"yes", "no"})
	rt.Complete(id, "done")
	key := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}

	// Retire past the horizon with a silent resolver.
	clk = clk.Add(protocol.DefaultCompactHorizon + time.Minute)
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	rec, _, _ := store.Get(key)
	if len(rec.RetiredOutcomes) != 1 || rec.RetiredOutcomes[0] != "i_5" {
		t.Fatalf("retired outcomes = %v, want [i_5]", rec.RetiredOutcomes)
	}

	// The harness then reports how the interaction really ended: a native
	// exact resolution must replace the uncertain one, not be dropped.
	if !rt.ResolveOutcome(id, "i_5", protocol.ResolutionAnswered, "yes") {
		t.Fatal("fake could not emit the late resolution")
	}
	rec, _, _ = store.Get(key)
	want := protocol.Resolution{InteractionID: "i_5", Outcome: protocol.ResolutionAnswered, Option: "yes"}
	if len(rec.RetiredOutcomes) != 0 || len(rec.Resolved) == 0 || rec.Resolved[len(rec.Resolved)-1] != want {
		t.Fatalf("after the late native event: retired=%v resolved=%+v, want %+v", rec.RetiredOutcomes, rec.Resolved, want)
	}
	if rec.State != protocol.StateCompleted || rec.Result == nil || rec.Result.Text != "done" {
		t.Fatalf("the correction changed the terminal record: state=%s result=%+v", rec.State, rec.Result)
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

// barrierResolver blocks its first ResolvedInteraction until released, then
// answers from its own map, so the test can reattach a replacement while
// the unlocked lookup of a Reconcile is in flight (bead 611.45 r3: the
// locked retirement must ask the CURRENT target, not the attachment
// captured before the lock).
type barrierResolver struct {
	core.Attachment
	mu      sync.Mutex
	res     map[string]protocol.Resolution
	entered chan struct{}
	release chan struct{}
}

func (b *barrierResolver) ResolvedInteraction(_ requests.Key, _, interactionID string) (protocol.Resolution, bool) {
	b.mu.Lock()
	res, ok := b.res[interactionID]
	entered, release := b.entered, b.release
	b.mu.Unlock()
	if entered != nil {
		close(entered)
		<-release
		b.mu.Lock()
		b.entered, b.release = nil, nil
		b.mu.Unlock()
	}
	return res, ok
}

// A replacement attachment that reports the exact outcome for the same
// key/epoch/interaction: the shape of a live reattach whose fresh seam
// already knows how the interaction ended.
func TestRetirementAsksTheCurrentAttachment(t *testing.T) {
	store, now := openStore(t)
	clk := now()
	rt := fake.New("fake", "e_1")
	old := &barrierResolver{
		Attachment: rt,
		res:        map[string]protocol.Resolution{},
		entered:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	ep := core.New(core.Config{Store: store, Now: func() time.Time { return clk }})
	ep.Register(old)

	id := "11111111-1111-4111-8111-111111111150"
	if _, err := ep.Handle(submitCmd(id), ownerShare); err != nil {
		t.Fatal(err)
	}
	rt.Question(id, "i_6", []string{"yes", "no"})
	rt.Complete(id, "done")

	// Past the horizon, with the old attachment silent.
	clk = clk.Add(protocol.DefaultCompactHorizon + time.Minute)

	// The old attachment's first ResolvedInteraction (the unlocked lookup
	// of the Reconcile) blocks; while it is blocked, a reattach replaces it
	// with a replacement that already reports the exact outcome. The old
	// attachment stays silent: the locked retirement pass must consult the
	// CURRENT target to see the resolution.
	go func() {
		<-old.entered
		replacement := &barrierResolver{
			Attachment: fake.New("fake", "e_1"),
			res: map[string]protocol.Resolution{
				"i_6": {InteractionID: "i_6", Outcome: protocol.ResolutionAnswered, Option: "yes"},
			},
		}
		ep.Register(replacement)
		close(old.release)
	}()

	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}

	key := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
	rec, _, err := store.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	// The replacement's exact resolution was recorded; nothing was retired.
	want := protocol.Resolution{InteractionID: "i_6", Outcome: protocol.ResolutionAnswered, Option: "yes"}
	if len(rec.RetiredOutcomes) != 0 || len(rec.Resolved) == 0 || rec.Resolved[len(rec.Resolved)-1] != want {
		t.Fatalf("after the reattach: retired=%v resolved=%+v, want %+v recorded with no retirement", rec.RetiredOutcomes, rec.Resolved, want)
	}
	if rec.State != protocol.StateCompleted || rec.Result == nil || rec.Result.Text != "done" {
		t.Fatalf("the correction changed the terminal record: state=%s result=%+v", rec.State, rec.Result)
	}
}
