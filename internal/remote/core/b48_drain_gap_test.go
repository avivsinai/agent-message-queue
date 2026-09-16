package core_test

import (
	"sync"
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
// Schedule (NATIVE-EVENT-ONLY, per codex round-3 P2): the submit Handle has
// RETURNED before the skipped-revision event fires. The only in-flight work
// when Close begins is the blocked native publication of the question
// snapshot; the newer revision arrives through rt.LocalAnswer
// (EventQuestionResolved -> onNative -> publishLocked) while that publish is
// held. No Handle/goroutine count can mask a skipped obligation.
//
//  1. Submit; run admits (StateRunning) and Handle returns after the
//     running publication.
//  2. rt.Question -> EventQuestion -> onNative commits revision r+1 and
//     publishes it; the publish BLOCKS inside the publisher (held on a
//     channel): publishing[key]=true, in-flight, drain counts it.
//  3. rt.LocalAnswer -> EventQuestionResolved -> onNative commits revision
//     r+2 and calls publishLocked while r+1's publish is still in flight ->
//     publishing[key] coalescing early return. The native event handler
//     returns (acked).
//  4. Close runs: the drain waits for the in-flight publication. RED on
//     2a5e583: Close returns with r+1 durable but never published. With the
//     fix, the finishing publisher chains the skipped newer revision (or the
//     drain covers it) so Close returns only after it is published, and
//     Close returns nil only when the drain discharged everything.
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
	// Delivery count PER REVISION (codex round-3 P2): a plain call counter
	// can pass even if the same snapshot is delivered twice and a newer one
	// skipped; per-revision counts make the assertion exact. The hold waits
	// for the submit Handle to have RETURNED (handleReturned), then holds the
	// next publication — the question snapshot from the EventQuestion native
	// path. Because every publication in this schedule flows through this one
	// publisher, the gate cannot strand an unexpected extra delivery: the
	// cleanup releases it, and the per-revision map records it for the
	// exact-count assertion below.
	var pubMu sync.Mutex
	perRevision := map[int64]int{}
	// The gate arms ONLY after the submit Handle has returned: every
	// publication before that (the running snapshot inside Handle) passes
	// unheld, and the first publication after arming — the question snapshot
	// from the EventQuestion native path — is the held one. The once-only
	// CAS means the chained publication of the newer revision passes through
	// unheld (it is counted, which is what the exact-count assertion checks).
	var gateArmed, heldOnce atomic.Bool
	heldPublish := func(s protocol.Snapshot, origin map[string]string) error {
		pubMu.Lock()
		perRevision[s.Revision]++
		pubMu.Unlock()
		if gateArmed.Load() && heldOnce.CompareAndSwap(false, true) {
			close(pubStarted)
			<-releasePub
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
	// The Handle runs to admission and its revision-3 running publication
	// passes through UNHELD (the gate is not armed yet — arming it before
	// Handle would deadlock the synchronous running publication inside
	// Handle itself). Awaiting Handle here is the POINT of the schedule: by
	// the time the blocked publication begins the submit handler has fully
	// returned, so NO outer Handle is in flight when Close runs — the drain
	// counts only the native publication (codex round-3 P2: no Handle
	// goroutine count may mask a skipped obligation).
	if _, err := ep.Handle(cmd, core.Source{Host: "local"}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	gateArmed.Store(true)
	if !b14cWait(func() bool { return rt.HasRun(id) }) {
		t.Fatal("never admitted")
	}

	// Revision r+1: question interaction publishes; block in the publisher.
	// The event must be raised from a goroutine: emit → onNative →
	// publishLocked runs the publisher INLINE on the emitting goroutine (the
	// realistic shape — native events arrive on the runtime's own thread), so
	// calling rt.Question synchronously would deadlock the test inside the
	// held publish before it can observe pubStarted.
	questionDone := make(chan struct{})
	go func() {
		defer close(questionDone)
		rt.Question(id, "i_1", []string{"yes", "no"})
	}()
	select {
	case <-pubStarted:
	case <-time.After(b48Timeout):
		close(releasePub)
		t.Fatal("interaction publication never started")
	}

	// Revision r+2: resolution event arrives while r+1's publication is
	// in-flight. onNative commits r+2, publishLocked early-returns into the
	// coalescing set (the bug), and the handler returns — the newer revision
	// is now acked but owes publication. The question event's goroutine is
	// still parked inside the held publish; the resolution's emit → onNative
	// → publishLocked hits the publishing[key] coalescing early-return and
	// does NOT block on the held publisher.
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

	// Close must drain BOTH the in-flight r+1 AND the skipped r+2.
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

	// THE assertion, BY REVISION (codex round-3 P2): the newest revision must
	// be published exactly once and the intermediate one exactly once. RED
	// on 2a5e583: the skipped r+2 never publishes, so its count is 0.
	k := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
	rec, _, _ := store.Get(k)
	pubMu.Lock()
	rev3 := perRevision[3]
	rev4 := perRevision[4]
	rev5 := perRevision[5]
	pubMu.Unlock()
	if rec.Revision != 5 {
		t.Fatalf("expected 5 durable revisions (received, dispatching/running, question, resolution), got %d", rec.Revision)
	}
	if rev3 != 1 {
		t.Fatalf("revision 3 (running) delivered %d times, want exactly 1 (duplicate/missed delivery)", rev3)
	}
	if rev4 != 1 {
		t.Fatalf("revision 4 (question) delivered %d times, want exactly 1 (duplicate/missed delivery)", rev4)
	}
	if rev5 == 0 {
		t.Fatalf("delivery counts by revision: running-pub=%d question-pub=%d resolution-pub=%d; PublishedRevision=%d (Astra B784-1: newer same-key revision skipped by the publishing[key] early-return escaped the shutdown drain; Close returned with it unpublished)",
			rev3, rev4, rev5, rec.PublishedRevision)
	}
	if rev5 > 1 {
		t.Fatalf("revision 5 (resolution) delivered %d times (duplicate delivery)", rev5)
	}
	if rec.PublishedRevision < rec.Revision {
		t.Fatalf("PublishedRevision=%d Revision=%d (chain published but marker did not advance)",
			rec.PublishedRevision, rec.Revision)
	}
}
