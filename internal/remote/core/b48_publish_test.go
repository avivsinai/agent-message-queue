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

// TestB48PublishDoesNotBlockConcurrentHandles reproduces
// agent-message-queue-611.22.48: carrier.Publish (the maildir open + fsync)
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
func TestB48PublishDoesNotBlockConcurrentHandles(t *testing.T) {
	store, now := openStoreNoCleanup(t)

	// Hold the first publisher on a channel; the second proceeds independently.
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
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

	// Wait until the first publisher is blocked inside the slow publish.
	<-firstStarted

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
	<-secondReached

	// Release the first so it can finish and both goroutines join.
	close(releaseFirst)
	wg.Wait()

	if got := atomic.LoadInt32(&pubCalls); got != 2 {
		t.Fatalf("expected 2 publish calls, got %d", got)
	}
}

// TestB48PendingPublishNotMarkedDelivered is codex's P1-1 regression: a
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
func TestB48PendingPublishNotMarkedDelivered(t *testing.T) {
	store, now := openStoreNoCleanup(t)

	pubStarted := make(chan struct{})
	releasePub := make(chan struct{})
	var calls int32
	heldPublish := func(s protocol.Snapshot, origin map[string]string) error {
		atomic.AddInt32(&calls, 1)
		close(pubStarted)
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
	<-pubStarted // the publish is now in-flight

	// A concurrent Reconcile for the same key/revision must NOT advance
	// PublishedRevision while the publish is in-flight (the revision is not
	// confirmed delivered yet). It must leave the work pending.
	_ = ep.Reconcile()
	k := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
	rec, _, _ := store.Get(k)
	if rec.PublishedRevision >= rec.Revision {
		t.Fatalf("PublishedRevision=%d advanced to Revision=%d while publish is still in-flight (611.22.48 P1-1: pending publication must not be marked delivered)", rec.PublishedRevision, rec.Revision)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected 1 publish call (the in-flight one), got %d (611.22.48 P1-1: concurrent Reconcile must not re-publish)", got)
	}

	// Release the held publish so the Handle completes and the revision is
	// confirmed delivered.
	close(releasePub)
	<-handleDone
	rec, _, _ = store.Get(k)
	if rec.PublishedRevision < rec.Revision {
		t.Fatalf("after publish completed, PublishedRevision=%d want >= %d (611.22.48)", rec.PublishedRevision, rec.Revision)
	}
}

// TestB48FailedPublishDoesNotClearNewerReservation is codex's P1-2 regression:
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
func TestB48FailedPublishDoesNotCorruptBookkeeping(t *testing.T) {
	store, now := openStoreNoCleanup(t)

	var calls int32
	failFirst := func(s protocol.Snapshot, origin map[string]string) error {
		if atomic.AddInt32(&calls, 1) == 1 {
			return errPublishFailed48
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
		t.Fatalf("failed publish advanced PublishedRevision=%d to Revision=%d (611.22.48 P1-2: failure must not advance bookkeeping)", rec.PublishedRevision, rec.Revision)
	}

	// Reconcile must retry the publish (the in-flight claim was cleared on
	// failure, visible was never set). The second call succeeds.
	if err := ep.Reconcile(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got < 2 {
		t.Fatalf("expected retry publish after reconcile, got %d calls (611.22.48: failed publish must release only its own in-flight claim so retry proceeds)", got)
	}
	rec, _, _ = store.Get(k)
	if rec.PublishedRevision < rec.Revision {
		t.Fatalf("after retry, PublishedRevision=%d want >= %d (611.22.48)", rec.PublishedRevision, rec.Revision)
	}
}

var errPublishFailed48 = protocol.Refuse(protocol.CodeNativeError, "publish failed for test")
