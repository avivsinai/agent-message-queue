package core_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// TestB48PublishDoesNotBlockConcurrentHandles reproduces
// agent-message-queue-611.22.48: carrier.Publish (the maildir open + fsync)
// ran under e.mu (endpoint.go:1027 publishLocked). No deadlock (the carrier
// never calls back in), but every Handle blocked on a disk sync, so two
// concurrent Handles on different records serialized behind the publisher.
//
// The fix moves publication OUTSIDE e.mu: publishLocked claims the
// visible-revision slot under the lock, snapshots the snapshot+origin,
// releases e.mu, calls e.publish, then re-acquires e.mu for the
// MarkPublished bookkeeping. Two Handles on different records now overlap in
// publication instead of serializing.
//
// Mutation RED: move the publish call back under e.mu (drop the
// Unlock/Lock around e.publish) -> the second Handle blocks behind the
// first's publish and the overlap assertion fails (elapsed ≈ 2×publish
// latency, not ≈ 1×).
func TestB48PublishDoesNotBlockConcurrentHandles(t *testing.T) {
	store, now := openStoreNoCleanup(t)

	// A publisher that sleeps to simulate a slow maildir open + fsync. The
	// sleep is long enough that serialized publication would be clearly
	// distinguishable from overlapped publication.
	const pubSleep = 150 * time.Millisecond
	var pubStarts []time.Time
	var pubMu sync.Mutex
	slowPublish := func(s protocol.Snapshot, origin map[string]string) error {
		pubMu.Lock()
		pubStarts = append(pubStarts, time.Now())
		pubMu.Unlock()
		time.Sleep(pubSleep)
		return nil
	}
	ep := core.New(core.Config{Store: store, Publish: slowPublish, Now: now})

	// Two distinct targets so both submits admit (the fake marks itself busy
	// after one admit, so a second submit to the SAME target returns CodeBusy
	// and takes a different publish path). Distinct targets guarantee both
	// reach the admitted-Running publishRevision.
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
	cmd1 := mkCmd("11111111-1111-4111-8111-111111111481", "t1")
	cmd2 := mkCmd("11111111-1111-4111-8111-111111111482", "t2")

	// Run both Handles concurrently. If publication is outside e.mu, their
	// publish calls overlap (start times within pubSleep); if under e.mu, the
	// second starts only after the first finishes (gap ≈ pubSleep).
	var wg sync.WaitGroup
	var err1, err2 error
	start := time.Now()
	wg.Add(2)
	go func() { defer wg.Done(); _, err1 = ep.Handle(cmd1, core.Source{Host: "local"}) }()
	go func() { defer wg.Done(); _, err2 = ep.Handle(cmd2, core.Source{Host: "local"}) }()
	wg.Wait()
	elapsed := time.Since(start)

	if err1 != nil {
		t.Fatalf("handle 1: %v", err1)
	}
	if err2 != nil {
		t.Fatalf("handle 2: %v", err2)
	}

	// Both publish calls must have fired (the admitted-Running revision).
	pubMu.Lock()
	starts := pubStarts
	pubMu.Unlock()
	if len(starts) != 2 {
		t.Fatalf("expected 2 publish calls, got %d", len(starts))
	}

	// Overlap assertion: the second publish started BEFORE the first finished.
	// If serialized (publish under e.mu), the gap between starts is >= pubSleep.
	gap := starts[1].Sub(starts[0])
	if gap >= pubSleep {
		t.Fatalf("publish calls serialized: gap between starts = %v, want < %v (611.22.48: publish must run outside e.mu so concurrent Handles overlap)", gap, pubSleep)
	}
	// Total elapsed must reflect overlap (~1×pubSleep), not serialization
	// (~2×pubSleep). Allow headroom for scheduling.
	if elapsed >= 2*pubSleep {
		t.Fatalf("handles serialized behind publish: elapsed = %v, want < %v (611.22.48)", elapsed, 2*pubSleep)
	}
}

// TestB48PublishFailureReleasesClaim proves the 611.22.48 failure path: when
// e.publish fails outside the lock, the visible-revision claim is released so
// the next reconcile retries (no permanent stall, no double-delivery risk).
//
// Mutation RED: on publish failure, do NOT delete(e.visible, key) -> the
// claim persists and a subsequent publishRevision for the same revision is
// skipped (visible[key] >= revision), so the retry never fires and the test
// fails (the second publish never happens).
func TestB48PublishFailureReleasesClaim(t *testing.T) {
	store, now := openStoreNoCleanup(t)

	var calls int32
	// Fail the first publish, succeed thereafter.
	failFirst := func(s protocol.Snapshot, origin map[string]string) error {
		if atomic.AddInt32(&calls, 1) == 1 {
			return errPublishFailed
		}
		return nil
	}
	ep := core.New(core.Config{Store: store, Publish: failFirst, Now: now})
	rt := fake.New("fake", "e_1")
	ep.Register(rt)

	cmd := &protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit,
		RequestID: "11111111-1111-4111-8111-111111111483", TargetID: "fake", Epoch: "e_1",
		NotAfter: protocol.FormatTime(now().Add(2 * time.Minute)),
		Input:    &protocol.SubmitInput{Text: "x"},
	}
	if _, err := ep.Handle(cmd, core.Source{Host: "local"}); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("expected 1 publish call after handle, got %d", calls)
	}

	// Reconcile must retry the publish (the claim was released on failure).
	// publishRevision fires from reconcileLive for the non-terminal record.
	_ = ep.Reconcile()
	if got := atomic.LoadInt32(&calls); got < 2 {
		t.Fatalf("expected retry publish after reconcile, got %d calls (611.22.48: failed publish must release the visible claim)", got)
	}
}

var errPublishFailed = protocol.Refuse(protocol.CodeNativeError, "publish failed for test")
