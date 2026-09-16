package core_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// TestB48CloseDrainsSkippedSameKeyRevision reproduces the Astra B784-1 finding
// on merged #784 (2a5e583): publishLocked early-returns when publishing[key]
// is set, so a NEWER accepted revision registers no pending-publication
// obligation and escapes the shutdown drain.
//
// Schedule (Astra's, via the native-event paths the fake supports):
//  1. Submit r1; run admits (StateRunning).
//  2. rt.Question -> EventQuestion -> onNative commits revision r2 and
//     publishes it; the publish BLOCKS inside the publisher (held on a
//     channel): publishing[key]=true, in-flight, drain counts it.
//  3. rt.LocalAnswer -> EventQuestionResolved -> onNative commits revision
//     r3 and calls publishLocked while r2's publish is still in flight ->
//     publishing[key] early return, NO obligation reserved. The native event
//     handler returns (acked).
//  4. Close runs: the drain waits for r2's publication only. RED on 2a5e583:
//     Close returns with r3 durable but never published. With the fix, the
//     finishing publisher chains the skipped newer revision (or the drain
//     covers it) so Close returns only after r3 is published.
func TestB48CloseDrainsSkippedSameKeyRevision(t *testing.T) {
	store, now := openStoreNoCleanup(t)

	pubStarted := make(chan struct{})
	releasePub := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-releasePub:
		default:
			close(releasePub)
		}
	})
	var completedRuns int32
	heldPublish := func(s protocol.Snapshot, origin map[string]string) error {
		// Hold ONLY the first non-submit publication (revision r2, the
		// EventQuestion interaction snapshot). The completion publication of
		// the submit Handle path (StateReceived/Dispatching on the Handle
		// goroutine) must pass through unblocked — the submit Handle runs to
		// admission BEFORE the question is raised, so gating on the running
		// state is unambiguous.
		if s.State == protocol.StateRunning {
			if atomic.AddInt32(&completedRuns, 1) == 1 {
				close(pubStarted)
				<-releasePub
				return nil
			}
			// r3's chained publication lands here with the fix.
		}
		return nil
	}
	ep := core.New(core.Config{Store: store, Publish: heldPublish, Now: now})
	rt := fake.New("fake", "e_1")
	ep.Register(rt)

	id := "11111111-1111-4111-8111-111111111486"
	cmd := &protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit,
		RequestID: id, TargetID: "fake", Epoch: "e_1",
		NotAfter: protocol.FormatTime(now().Add(2 * time.Minute)),
		Input:    &protocol.SubmitInput{Text: "x"},
	}
	handleDone := make(chan error, 1)
	go func() {
		handleDone <- func() error { _, err := ep.Handle(cmd, core.Source{Host: "local"}); return err }()
	}()
	if !b14cWait(func() bool { return rt.HasRun(id) }) {
		t.Fatal("never admitted")
	}

	// Revision r2: question interaction publishes; block in the publisher.
	rt.Question(id, "i_1", []string{"yes", "no"})
	select {
	case <-pubStarted:
	case <-time.After(b48Timeout):
		close(releasePub)
		t.Fatal("interaction publication never started")
	}

	// (The submit Handle's own running-publication is the one held in the
	// publisher; Handle itself is still blocked until releasePub — that's
	// the in-flight publication, equivalent to Astra's blocked revision r.)
	// Revision r3: resolution event arrives while r2's publication is
	// in-flight. onNative commits r3, publishLocked early-returns (the bug),
	// and the handler returns — the newer revision is now acked but owes
	// publication.
	resolvedDone := make(chan struct{})
	go func() {
		defer close(resolvedDone)
		rt.LocalAnswer("i_1", "yes")
	}()
	select {
	case <-resolvedDone:
	case <-time.After(b48Timeout):
		t.Fatal("resolution handler did not return")
	}

	// Close must drain BOTH the in-flight r2 AND the skipped r3.
	closeDone := make(chan error, 1)
	go func() { closeDone <- ep.Close() }()
	waitForDraining48(t, ep)
	close(releasePub)

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("close returned error: %v", err)
		}
	case <-time.After(b48Timeout):
		close(releasePub)
		t.Fatal("close did not return after publications released")
	}

	// THE assertion: the newest revision must be published before Close
	// returned. RED on 2a5e583: the skipped r3 never publishes, so
	// PublishedRevision < Revision.
	k := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
	rec, _, _ := store.Get(k)
	if rec.PublishedRevision < rec.Revision {
		t.Fatalf("PublishedRevision=%d Revision=%d (Astra B784-1: newer same-key revision skipped by the publishing[key] early-return escaped the shutdown drain; Close returned with it unpublished)",
			rec.PublishedRevision, rec.Revision)
	}
	if got := atomic.LoadInt32(&completedRuns); got < 2 {
		t.Fatalf("running-state publish calls=%d, want >=2 (the skipped revision must be published by the chain/drain, not left pending)", got)
	}
}
