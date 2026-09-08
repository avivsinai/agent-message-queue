package core_test

import (
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

func openStore(t *testing.T) (*requests.Store, func() time.Time) {
	clk := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	now := func() time.Time { return clk }
	store, err := requests.Open(t.TempDir(), requests.WithClock(now))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, now
}

func submitCmd(id string) *protocol.Command {
	return &protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit,
		RequestID: id, TargetID: "fake", Epoch: "e_1",
		NotAfter: protocol.FormatTime(time.Date(2026, 9, 8, 10, 2, 0, 0, time.UTC)),
		Input:    &protocol.SubmitInput{Text: "x"},
	}
}

// TestReconcilePoisonRecordResolvesNotAborts reproduces the P0 where a record
// left uncertain (target briefly unregistered) then re-examined with the
// target back but no run bound would attempt uncertain -> rejected, which the
// store refused, aborting the whole reconcile pass and serve startup.
func TestReconcilePoisonRecordResolvesNotAborts(t *testing.T) {
	store, now := openStore(t)
	// Seed an uncertain record with no run, plus a healthy second record, so
	// a per-record failure would starve the second one too.
	for _, id := range []string{"11111111-1111-4111-8111-1111111111a1", "11111111-1111-4111-8111-1111111111a2"} {
		rec := &requests.Record{Snapshot: protocol.Snapshot{
			Schema: protocol.SchemaRequest, RequestID: id, CreatorHost: "local",
			TargetID: "fake", Epoch: "e_1", Revision: 1, State: protocol.StateReceived,
			InputDigest: requests.Digest([]byte("x")), NotAfter: protocol.FormatTime(now().Add(2 * time.Minute)),
			ObservedAt: protocol.FormatTime(now()),
		}}
		if err := store.Create(rec); err != nil {
			t.Fatal(err)
		}
		rec.Revision, rec.State, rec.NativeDispatches = 2, protocol.StateDispatching, 1
		if err := store.Update(rec); err != nil {
			t.Fatal(err)
		}
		rec.Revision, rec.State, rec.Code = 3, protocol.StateUncertain, protocol.CodeAttachmentLost
		if err := store.Update(rec); err != nil {
			t.Fatal(err)
		}
	}
	ep := core.New(core.Config{Store: store, Now: now})
	ep.Register(fake.New("fake", "e_1")) // live target, but no run for either key
	if err := ep.Reconcile(); err != nil {
		t.Fatalf("reconcile aborted on a poison record: %v", err)
	}
	for _, id := range []string{"11111111-1111-4111-8111-1111111111a1", "11111111-1111-4111-8111-1111111111a2"} {
		rec, _, err := store.Get(requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id})
		if err != nil {
			t.Fatal(err)
		}
		if rec.State != protocol.StateRejected {
			t.Fatalf("record %s stuck at %s, want rejected", id, rec.State)
		}
	}
}

// TestReconcileDoesNotHoldLockAcrossNativeCalls reproduces the P0 where
// Reconcile held the endpoint mutex across a native Lookup, stalling every
// other operation. A concurrent Handle must complete while the Lookup blocks.
func TestReconcileDoesNotHoldLockAcrossNativeCalls(t *testing.T) {
	store, now := openStore(t)
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: store, Now: now})
	ep.Register(rt)

	// One running record for Reconcile to Lookup.
	if _, err := ep.Handle(submitCmd("11111111-1111-4111-8111-1111111111b1"), core.Source{Host: "local"}); err != nil {
		t.Fatalf("seed submit: %v", err)
	}

	rt.HoldLookup()
	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- ep.Reconcile() }()
	time.Sleep(20 * time.Millisecond) // let Reconcile reach the blocked Lookup

	handled := make(chan error, 1)
	go func() {
		_, err := ep.Handle(&protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpSessionList}, core.Source{Host: "local"})
		handled <- err
	}()
	select {
	case err := <-handled:
		if err != nil {
			t.Fatalf("concurrent Handle failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Handle blocked while Reconcile held the lock across a native Lookup")
	}
	rt.ReleaseLookup()
	if err := <-reconcileDone; err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

// TestCancelBeforeAdmissionConfirmsDisposition reproduces the P1 where a cancel
// racing a gated admission left a cancelled record whose cancel disposition
// still read cancel_requested.
func TestCancelBeforeAdmissionConfirmsDisposition(t *testing.T) {
	store, now := openStore(t)
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: store, Now: now})
	ep.Register(rt)

	rt.HoldAdmission()
	id := "11111111-1111-4111-8111-1111111111c1"
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _, _ = ep.Handle(submitCmd(id), core.Source{Host: "local"}) }()
	time.Sleep(20 * time.Millisecond) // submit reaches the held admission gate

	ref := protocol.EncodeRef("local", "fake", id)
	if _, err := ep.Handle(&protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpRequestCancel, RequestRef: ref,
		TargetID: "fake", Epoch: "e_1", NotAfter: protocol.FormatTime(now().Add(2 * time.Minute)),
	}, core.Source{Host: "local"}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	rt.ReleaseAdmission()
	wg.Wait()

	rec, _, err := store.Get(requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id})
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != protocol.StateCancelled {
		t.Fatalf("state %s, want cancelled", rec.State)
	}
	if rec.Cancel == nil || rec.Cancel.Disposition != protocol.CancelConfirmed {
		t.Fatalf("cancelled record has non-confirmed disposition: %+v", rec.Cancel)
	}
	if rt.Snapshot().Dispatches != 1 {
		t.Fatalf("native dispatches = %d, want 1", rt.Snapshot().Dispatches)
	}
}

// TestRespondReplayAnswersOnce reproduces the P2: an AMQ-level replay of an
// interaction answer must not invoke the attachment twice.
func TestRespondReplayAnswersOnce(t *testing.T) {
	store, now := openStore(t)
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: store, Now: now})
	ep.Register(rt)

	id := "11111111-1111-4111-8111-1111111111d1"
	if _, err := ep.Handle(submitCmd(id), core.Source{Host: "local"}); err != nil {
		t.Fatal(err)
	}
	rt.Question(id, "i_1", []string{"yes", "no"})
	ref := protocol.EncodeRef("local", "fake", id)
	respond := &protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpInteractionRespond, RequestRef: ref,
		TargetID: "fake", Epoch: "e_1", InteractionID: "i_1", Option: "yes",
	}
	if _, err := ep.Handle(respond, core.Source{Host: "local"}); err != nil {
		t.Fatalf("respond: %v", err)
	}
	// Replay the identical answer, as an AMQ re-import would after a crash.
	if _, err := ep.Handle(respond, core.Source{Host: "local"}); err != nil {
		t.Fatalf("respond replay: %v", err)
	}
	answers := rt.Snapshot().Answers
	if len(answers) != 1 || answers[0].InteractionID != "i_1" || answers[0].Option != "yes" {
		t.Fatalf("interaction answered %d times: %+v", len(answers), answers)
	}
}
