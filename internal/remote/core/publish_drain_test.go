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

// publishWaitTimeout is the bound on every channel wait in these tests. A correct
// implementation never hits it; a regression (hang) fails the test instead of
// stranding the suite.
const publishWaitTimeout = 10 * time.Second

// TestPublishDoesNotBlockConcurrentHandles reproduces #784: carrier.Publish (the maildir open + fsync)
// ran under e.mu, so every Handle blocked on a disk sync and two concurrent
// Handles on different records serialized behind the publisher.
//
// The fix moves publication OUTSIDE e.mu. This test holds the first
// publisher on a channel and verifies an unrelated Handle (different key)
// reaches publication BEFORE the first is released — proving the two overlap
// rather than serialize behind the global mutex. No timing/sleep assertions.
//
// Mutation RED: move the publish call back under e.mu (drop the Unlock/Lock
// around e.publish) -> the second Handle cannot reach publication until the
// first releases, so the "second reached publication before first released"
// assertion fails.
func TestPublishDoesNotBlockConcurrentHandles(t *testing.T) {
	store, now := openStoreNoCleanup(t)

	// Hold the first publisher on a channel; the second proceeds independently.
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	// Unconditional safe release: if a fatal assertion strands the held
	// publisher, t.Cleanup releases it so the suite does not hang.
	t.Cleanup(func() {
		select {
		case <-releaseFirst:
		default:
			close(releaseFirst)
		}
	})
	var firstMu sync.Mutex
	firstKey := "11111111-1111-4111-8111-111111111481"
	var pubCalls int32
	slowPublish := func(s protocol.Snapshot, origin map[string]string) error {
		atomic.AddInt32(&pubCalls, 1)
		firstMu.Lock()
		isFirst := s.RequestID == firstKey
		firstMu.Unlock()
		if isFirst {
			close(firstStarted)
			<-releaseFirst
		}
		return nil
	}
	ep := core.New(core.Config{Store: store, Publish: slowPublish, Now: now})

	// Two distinct targets so both submits admit (the fake marks itself busy
	// after one admit; a second submit to the SAME target returns CodeBusy).
	rt1 := fake.New("t1", "e_1")
	ep.Register(rt1)
	rt2 := fake.New("t2", "e_1")
	ep.Register(rt2)

	mkCmd := func(id, target string) *protocol.Command {
		return &protocol.Command{
			Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit,
			RequestID: id, TargetID: target, Epoch: "e_1",
			NotAfter: protocol.FormatTime(now().Add(2 * time.Minute)),
			Input:    &protocol.SubmitInput{Text: "x"},
		}
	}

	// Start the first Handle (held in publication via the channel).
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = ep.Handle(mkCmd(firstKey, "t1"), core.Source{Host: "local"}) }()

	// Wait (bounded) until the first publisher is blocked inside the slow publish.
	select {
	case <-firstStarted:
	case <-time.After(publishWaitTimeout):
		close(releaseFirst)
		t.Fatal("first publisher never started")
	}

	// Start the second Handle on a DIFFERENT key/target. It must reach
	// publication while the first is still held (publish outside e.mu).
	secondReached := make(chan struct{})
	go func() {
		defer wg.Done()
		_, _ = ep.Handle(mkCmd("11111111-1111-4111-8111-111111111482", "t2"), core.Source{Host: "local"})
		close(secondReached)
	}()

	// The second Handle must reach publication (and complete) while the first
	// is still held. If publish is under e.mu, the second blocks behind the
	// first and secondReached never fires before releaseFirst.
	select {
	case <-secondReached:
		// expected: the second Handle completed while the first was held.
	case <-time.After(publishWaitTimeout):
		close(releaseFirst)
		t.Fatal("second Handle did not reach publication while first was held (publish under e.mu)")
	}

	// Release the first so it can finish and both goroutines join (bounded).
	close(releaseFirst)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(publishWaitTimeout):
		t.Fatal("handles did not join after release (hang)")
	}

	if got := atomic.LoadInt32(&pubCalls); got != 2 {
		t.Fatalf("expected 2 publish calls, got %d", got)
	}
}

// TestPendingPublishNotMarkedDelivered is a regression: a
// pending (in-flight) publication must NOT be marked as delivered. While A is
// blocked publishing revision r, a concurrent Reconcile for r must NOT skip
// the publish and fall through to MarkPublished(r) — that would lose the
// revision if A fails. With the fix, the `publishing` in-flight set makes the
// concurrent caller leave the work pending (return early), so
// PublishedRevision is NOT advanced while A is in flight.
//
// Mutation RED: revert to visible[key]=r before publish (no `publishing` set)
// -> the concurrent Reconcile sees visible>=r, skips publish, and
// MarkPublished(r) fires while A is still in flight; if A then fails,
// PublishedRevision is already r and the assertion fails.
func TestPendingPublishNotMarkedDelivered(t *testing.T) {
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
	var calls int32
	var pubStartedOnce sync.Once
	heldPublish := func(s protocol.Snapshot, origin map[string]string) error {
		atomic.AddInt32(&calls, 1)
		// The finishing publisher now chains skipped revisions
		// (a second publishRevision for the same key), so the held publish
		// can run more than once. close() must be once-only; every entry
		// holds until releasePub (drain is serialized per key, so holders
		// queue and all complete when it closes).
		pubStartedOnce.Do(func() { close(pubStarted) })
		<-releasePub
		return nil
	}
	ep := core.New(core.Config{Store: store, Publish: heldPublish, Now: now})
	rt := fake.New("fake", "e_1")
	ep.Register(rt)

	id := "11111111-1111-4111-8111-111111111483"
	cmd := &protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit,
		RequestID: id, TargetID: "fake", Epoch: "e_1",
		NotAfter: protocol.FormatTime(now().Add(2 * time.Minute)),
		Input:    &protocol.SubmitInput{Text: "x"},
	}

	// Start the Handle; it blocks inside the held publish (in-flight).
	handleDone := make(chan struct{})
	go func() { defer close(handleDone); _, _ = ep.Handle(cmd, core.Source{Host: "local"}) }()
	// Wait (bounded) for the publish to be in-flight.
	select {
	case <-pubStarted:
	case <-time.After(publishWaitTimeout):
		close(releasePub)
		t.Fatal("publish never started")
	}

	// A concurrent Reconcile for the same key/revision must NOT advance
	// PublishedRevision while the publish is in-flight (the revision is not
	// confirmed delivered yet). It must leave the work pending. Reconcile runs
	// in a goroutine with a BOUNDED join: with the held
	// publish, a regression that re-publishes inside Reconcile would block it
	// on releasePub forever — the test must fail on timeout, not hang the
	// suite.
	reconcileDone := make(chan struct{})
	go func() { defer close(reconcileDone); _ = ep.Reconcile() }()
	select {
	case <-reconcileDone:
	case <-time.After(publishWaitTimeout):
		close(releasePub)
		t.Fatal("reconcile did not return while publish is in-flight (it must leave the work pending, not block behind the held publisher)")
	}
	k := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
	rec, _, _ := store.Get(k)
	if rec.PublishedRevision >= rec.Revision {
		t.Fatalf("PublishedRevision=%d advanced to Revision=%d while publish is still in-flight (pending publication must not be marked delivered)", rec.PublishedRevision, rec.Revision)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected 1 publish call (the in-flight one), got %d (concurrent Reconcile must not re-publish)", got)
	}

	// Release the held publish so the Handle completes and the revision is
	// confirmed delivered.
	close(releasePub)
	select {
	case <-handleDone:
	case <-time.After(publishWaitTimeout):
		t.Fatal("handle did not return after publish released")
	}
	rec, _, _ = store.Get(k)
	if rec.PublishedRevision < rec.Revision {
		t.Fatalf("after publish completed, PublishedRevision=%d want >= %d", rec.PublishedRevision, rec.Revision)
	}
	// Duplicate-delivery check AFTER completion: the
	// post-release assertions must verify the revision was delivered exactly
	// once in total, not just that the marker advanced. A regression that
	// re-publishes r after the original r succeeds (same-revision chain)
	// shows up as calls=2 here.
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("after completion, publish calls=%d, want exactly 1 (revision %d delivered more than once)", got, rec.Revision)
	}
}

// TestFailedPublishDoesNotCorruptBookkeeping is a regression:
// an older attempt's cleanup must NOT clear a newer attempt's reservation.
// A publishes revision r and fails. Before A clears its in-flight claim, B
// cannot start r+1 (per-key serialization), so this test verifies the
// narrower property: A's failure cleanup deletes only A's `publishing` entry
// and does not advance or corrupt visible/PublishedRevision, so the next
// Reconcile retries r cleanly. A subsequent successful publish for the same
// key then proceeds.
//
// Mutation RED: on failure, delete(e.visible, key) unconditionally (the old
// code) -> if a concurrent caller had advanced visible, A clobbers it. With
// the fix, A's failure never touches visible (only clears its own
// `publishing` entry), so the retry is clean.
func TestFailedPublishDoesNotCorruptBookkeeping(t *testing.T) {
	store, now := openStoreNoCleanup(t)

	var calls int32
	failFirst := func(s protocol.Snapshot, origin map[string]string) error {
		if atomic.AddInt32(&calls, 1) == 1 {
			return errPublishFailed
		}
		return nil
	}
	ep := core.New(core.Config{Store: store, Publish: failFirst, Now: now})
	rt := fake.New("fake", "e_1")
	ep.Register(rt)

	id := "11111111-1111-4111-8111-111111111484"
	cmd := &protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit,
		RequestID: id, TargetID: "fake", Epoch: "e_1",
		NotAfter: protocol.FormatTime(now().Add(2 * time.Minute)),
		Input:    &protocol.SubmitInput{Text: "x"},
	}
	if _, err := ep.Handle(cmd, core.Source{Host: "local"}); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected 1 publish call after handle, got %d", got)
	}

	k := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
	rec, _, _ := store.Get(k)
	if rec.PublishedRevision >= rec.Revision {
		t.Fatalf("failed publish advanced PublishedRevision=%d to Revision=%d (failure must not advance bookkeeping)", rec.PublishedRevision, rec.Revision)
	}

	// Reconcile must retry the publish (the in-flight claim was cleared on
	// failure, visible was never set). The second call succeeds.
	if err := ep.Reconcile(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got < 2 {
		t.Fatalf("expected retry publish after reconcile, got %d calls (failed publish must release only its own in-flight claim so retry proceeds)", got)
	}
	rec, _, _ = store.Get(k)
	if rec.PublishedRevision < rec.Revision {
		t.Fatalf("after retry, PublishedRevision=%d want >= %d", rec.PublishedRevision, rec.Revision)
	}
}

// TestCloseDrainsAsyncNativePublication is a regression:
// the onNative callback path (EventRunCompleted -> publishLocked) is not a
// Handle, so it was not counted in inFlight. While its publish now runs
// outside e.mu, Close could observe zero handlers, close the store, and
// return before publication finished — a successful publish could not then
// MarkPublished (store closed), leaving a duplicate on restart or exiting
// with publication still running.
//
// The fix accounts for the accepted publication work in the bounded shutdown
// drain: publishLocked increments inFlight before releasing e.mu for the
// publish and decrements after re-acquiring, so Close's drain waits for the
// publication window (preserving the unlocked publisher).
//
// This test holds an async native-event publication on a channel while Close
// runs. Close must NOT return before the publish completes and MarkPublished
// lands. No timing/sleep assertions.
//
// Mutation RED: remove the inFlight++/drain accounting in publishLocked ->
// Close observes inFlight==0, closes the store, and returns before the
// publish completes; MarkPublished fails (store closed) and the assertion
// fails.
// Regression: bead agent-message-queue-611.22.48.
func TestCloseDrainsAsyncNativePublication(t *testing.T) {
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
	var pubCalls int32
	heldPublish := func(s protocol.Snapshot, origin map[string]string) error {
		atomic.AddInt32(&pubCalls, 1)
		// Only hold the COMPLETION publication (the async native-event path via
		// onNative). Submit-time publications (Received/Dispatching) must not be
		// held or the Handle itself deadlocks.
		if s.State == protocol.StateCompleted {
			close(pubStarted)
			<-releasePub
		}
		return nil
	}
	ep := core.New(core.Config{Store: store, Publish: heldPublish, Now: now})
	rt := fake.New("fake", "e_1")
	ep.Register(rt)

	id := "11111111-1111-4111-8111-111111111485"
	if _, err := ep.Handle(submitCmd(id), core.Source{Host: "local"}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !waitUntil(func() bool { return rt.HasRun(id) }) {
		t.Fatal("never admitted")
	}

	// rt.Complete emits EventRunCompleted -> onNative -> publishLocked, which
	// blocks inside the held publish (async native-event publication). Run it
	// in a goroutine so the test can observe Close draining while it is held.
	completeDone := make(chan struct{})
	go func() {
		defer close(completeDone)
		rt.Complete(id, "done")
	}()
	select {
	case <-pubStarted:
	case <-time.After(publishWaitTimeout):
		close(releasePub)
		t.Fatal("async native publication never started")
	}

	// Run Close concurrently. It must NOT return before the held publish
	// completes: the publication work is counted in inFlight, so Close's drain
	// waits. closeDone must stay blocked until releasePub.
	closeDone := make(chan error, 1)
	go func() { closeDone <- ep.Close() }()
	// Synchronize on the ACTUAL draining state (not a 100ms absence check,
	// and not IsDraining which conflates draining+closed): poll
	// IsDrainingState() until Close has transitioned to draining. This proves
	// Close entered the drain wait while the publish is still in-flight, so
	// the lifecycle accounting is exercised regardless of scheduling. If
	// Close reached stateClosed here, it raced past the drain without waiting
	// (a false pass).
	waitForDrainingState(t, ep)
	// Close must still be blocked (the held publish keeps inFlight > 0).
	select {
	case err := <-closeDone:
		close(releasePub)
		t.Fatalf("close returned before publication completed (Close must drain accepted publication work): %v", err)
	default:
		// expected: Close is blocked in the drain wait.
	}

	// Release the held publish. Close can now drain and return.
	close(releasePub)
	select {
	case <-completeDone:
	case <-time.After(publishWaitTimeout):
		t.Fatal("rt.Complete did not return after publish released")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("close returned error: %v", err)
		}
	case <-time.After(publishWaitTimeout):
		t.Fatal("close did not return after publication completed")
	}

	// The publish completed BEFORE Close closed the store (Close waited), so
	// MarkPublished landed: PublishedRevision advanced to Revision.
	k := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
	rec, _, _ := store.Get(k)
	if rec.PublishedRevision < rec.Revision {
		t.Fatalf("PublishedRevision=%d want >= %d (Close must not close the store before MarkPublished lands)", rec.PublishedRevision, rec.Revision)
	}
}

var errPublishFailed = protocol.Refuse(protocol.CodeNativeError, "publish failed for test")

// waitForDrainingState polls IsDrainingState() until the endpoint has entered
// the DRAINING state specifically (not closed). This is the close-wait
// ordering fix: IsDraining() conflates draining and closed, so a
// test that released a held publish after observing IsDraining()==true could
// be observing stateClosed — Close raced past the drain wait without actually
// waiting. IsDrainingState() fails if Close reached stateClosed before the
// publish was released, proving Close is genuinely in the drain wait.
func waitForDrainingState(t *testing.T, ep *core.Endpoint) {
	t.Helper()
	deadline := time.Now().Add(publishWaitTimeout)
	for time.Now().Before(deadline) {
		if ep.IsDrainingState() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for endpoint to enter draining state (not closed)")
}

// TestCloseWaitsFailedPublishReturnsNil is a regression for the failure branch in the close-wait ordering. A held publish FAILS;
// Close must enter the DRAINING state (not race to stateClosed) while the
// publish is in flight, then return nil because a failed attempt of the
// published revision is NOT an undischarged obligation (the attempt ran and
// was counted; the durable unpublished state persists for Reconcile recovery).
// ErrDrainIncomplete would arise only if a coalesced NEWER r+1 was dropped.
// This exercises the failed-r path in the close-wait regression (the previous
// close-wait test's publisher always returned nil).
func TestCloseWaitsFailedPublishReturnsNil(t *testing.T) {
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
	// The gate arms ONLY after the submit Handle has returned: every
	// publication before that (the running snapshot inside Handle) passes
	// unheld, and the first publication after arming — the completed snapshot
	// from the EventRunCompleted native path — is the held one that fails.
	var gateArmed atomic.Bool
	var heldOnce atomic.Bool
	heldPublish := func(s protocol.Snapshot, origin map[string]string) error {
		if gateArmed.Load() && heldOnce.CompareAndSwap(false, true) {
			close(pubStarted)
			<-releasePub
			return errPublishFailed
		}
		return nil
	}
	ep := core.New(core.Config{Store: store, Publish: heldPublish, Now: now})
	rt := fake.New("fake", "e_1")
	ep.Register(rt)

	id := "11111111-1111-4111-8111-111111111488"
	if _, err := ep.Handle(submitCmd(id), core.Source{Host: "local"}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !waitUntil(func() bool { return rt.HasRun(id) }) {
		t.Fatal("never admitted")
	}
	// Arm the gate AFTER Handle returned and the run is admitted: the next
	// publication (the completed snapshot from rt.Complete's native event) is
	// the held one that fails.
	gateArmed.Store(true)

	completeDone := make(chan struct{})
	go func() {
		defer close(completeDone)
		rt.Complete(id, "done")
	}()
	select {
	case <-pubStarted:
	case <-time.After(publishWaitTimeout):
		close(releasePub)
		t.Fatal("failed-r publication never started")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- ep.Close() }()
	// Close must enter DRAINING (not stateClosed) while the publish is held.
	// IsDrainingState() fails if Close raced to stateClosed (a
	// false pass: IsDraining conflates draining+closed).
	waitForDrainingState(t, ep)
	select {
	case err := <-closeDone:
		close(releasePub)
		t.Fatalf("close returned before failed publication completed: %v", err)
	default:
	}

	// Release the held publish; it fails. Close drains and returns nil:
	// a failed attempt of attemptRev itself is NOT an undischarged
	// obligation (the attempt ran and was counted; the durable unpublished
	// state persists for Reconcile recovery — the contract-corpus Q18
	// restart flow relies on Close returning nil here). ErrDrainIncomplete
	// would arise only if a coalesced NEWER r+1 was dropped, which this
	// schedule does not produce.
	close(releasePub)
	select {
	case <-completeDone:
	case <-time.After(publishWaitTimeout):
		t.Fatal("rt.Complete did not return after publish released")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("close returned error after failed publication; want nil (failed attempt is not an undischarged obligation): %v", err)
		}
	case <-time.After(publishWaitTimeout):
		t.Fatal("close did not return after failed publication completed")
	}

	// The failed publish did NOT advance PublishedRevision (failure must not
	// mark); the durable unpublished state persists for Reconcile recovery.
	k := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
	rec, _, _ := store.Get(k)
	if rec.PublishedRevision >= rec.Revision {
		t.Fatalf("failed publish advanced PublishedRevision=%d to Revision=%d", rec.PublishedRevision, rec.Revision)
	}
}

// TestCloseHoldsFailedPublishRetriesCoalescedRevision is a regression:
// hold revision r (the question publication), coalesce r+1 (the resolution)
// while r is held, then FAIL r. Close must attempt r+1 (the coalesced newer
// revision) and mark it published before returning nil. This proves the
// drain does not skip a coalesced newer revision when the held older one
// fails — the obligation for r+1 is discharged by the retry, not lost.
func TestCloseHoldsFailedPublishRetriesCoalescedRevision(t *testing.T) {
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

	// Track publication attempts per revision.
	var pubMu sync.Mutex
	perRevision := make(map[int64]int)
	// The gate arms ONLY after the submit Handle has returned: the running
	// snapshot inside Handle publishes unheld, and the first publication
	// after arming (the question event) is the held one that fails.
	var gateArmed atomic.Bool
	var heldOnce atomic.Bool
	heldPublish := func(s protocol.Snapshot, origin map[string]string) error {
		pubMu.Lock()
		perRevision[s.Revision]++
		pubMu.Unlock()
		if gateArmed.Load() && heldOnce.CompareAndSwap(false, true) {
			close(pubStarted)
			<-releasePub
			return errPublishFailed
		}
		return nil
	}
	ep := core.New(core.Config{Store: store, Publish: heldPublish, Now: now})
	rt := fake.New("fake", "e_1")
	ep.Register(rt)

	id := "11111111-1111-4111-8111-111111111489"
	if _, err := ep.Handle(submitCmd(id), core.Source{Host: "local"}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !waitUntil(func() bool { return rt.HasRun(id) }) {
		t.Fatal("never admitted")
	}

	// Arm the gate: the next publication (the question event) is the held
	// one that fails.
	gateArmed.Store(true)

	questionDone := make(chan struct{})
	go func() {
		defer close(questionDone)
		rt.Question(id, "i_1", []string{"yes", "no"})
	}()
	select {
	case <-pubStarted:
	case <-time.After(publishWaitTimeout):
		close(releasePub)
		t.Fatal("question publication (r) never started")
	}

	// While r is held, coalesce r+1: the resolution event arrives and
	// publishLocked early-returns into the coalescing set (the held publish
	// is still in flight). This creates an undischarged obligation for r+1.
	resolvedDone := make(chan struct{})
	go func() {
		defer close(resolvedDone)
		rt.LocalAnswer("i_1", "yes")
	}()
	select {
	case <-resolvedDone:
	case <-time.After(publishWaitTimeout):
		t.Fatal("resolution handler did not return")
	}

	// Close must drain both the in-flight r and the coalesced r+1.
	closeDone := make(chan error, 1)
	go func() { closeDone <- ep.Close() }()
	waitForDrainingState(t, ep)

	// Release the held publish; it fails (r). Close must then attempt r+1
	// (the coalesced newer revision) and mark it published before returning.
	close(releasePub)

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("close returned error after failed r + coalesced r+1; want nil (r+1 should have been published): %v", err)
		}
	case <-time.After(publishWaitTimeout):
		t.Fatal("close did not return after publications released")
	}

	// Bound the question goroutine.
	select {
	case <-questionDone:
	case <-time.After(publishWaitTimeout):
		t.Fatal("rt.Question goroutine did not return after Close (leaked)")
	}

	// r+1 (the resolution revision) must have been attempted at least once
	// and PublishedRevision must have advanced to Revision.
	pubMu.Lock()
	revs := make([]int64, 0, len(perRevision))
	for r := range perRevision {
		revs = append(revs, r)
	}
	pubMu.Unlock()

	k := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
	rec, _, _ := store.Get(k)
	if rec.PublishedRevision < rec.Revision {
		t.Fatalf("coalesced r+1 was not published: PublishedRevision=%d Revision=%d (attempts: %v)",
			rec.PublishedRevision, rec.Revision, revs)
	}
}

// TestCloseDrainsSkippedSameKeyRevision reproduces #784: publishLocked early-returns when publishing[key]
// is set, so a NEWER accepted revision registers no pending-publication
// obligation and escapes the shutdown drain.
//
// Schedule (NATIVE-EVENT-ONLY): the submit Handle has
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
//  4. Close runs: the drain waits for the in-flight publication. Before
//     the fix, Close returns with r+1 durable but never published. With the
//     fix, the finishing publisher chains the skipped newer revision (or the
//     drain covers it) so Close returns only after it is published, and
//     Close returns nil only when the drain discharged everything.
func TestCloseDrainsSkippedSameKeyRevision(t *testing.T) {
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
	// Delivery count PER REVISION: a plain call counter
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
	// counts only the native publication (no Handle
	// goroutine count may mask a skipped obligation).
	if _, err := ep.Handle(cmd, core.Source{Host: "local"}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	gateArmed.Store(true)
	if !waitUntil(func() bool { return rt.HasRun(id) }) {
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
	case <-time.After(publishWaitTimeout):
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
	case <-time.After(publishWaitTimeout):
		t.Fatal("resolution handler did not return")
	}

	// Close must drain BOTH the in-flight r+1 AND the skipped r+2.
	closeDone := make(chan error, 1)
	go func() { closeDone <- ep.Close() }()
	waitForDrainingState(t, ep)
	close(releasePub)

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("close returned error: %v", err)
		}
	case <-time.After(publishWaitTimeout):
		close(releasePub)
		t.Fatal("close did not return after publications released")
	}

	// Bound the question goroutine: after Close drains and returns, the held
	// publish has been released, so rt.Question must return. A leak here means
	// a goroutine is still parked inside the publisher (a deadlock the test
	// must surface, not silently leak).
	select {
	case <-questionDone:
	case <-time.After(publishWaitTimeout):
		t.Fatal("rt.Question goroutine did not return after Close (leaked)")
	}

	// THE assertion, BY REVISION: the newest revision must
	// be published exactly once and the intermediate one exactly once. Before
	// the fix, the skipped r+2 never publishes, so its count is 0.
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
		t.Fatalf("delivery counts by revision: running-pub=%d question-pub=%d resolution-pub=%d; PublishedRevision=%d (newer same-key revision skipped by the publishing[key] early-return escaped the shutdown drain; Close returned with it unpublished)",
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
