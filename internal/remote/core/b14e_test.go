package core_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// rejectedRecord creates a terminal, published record with no result in one
// write: tombstone marks it as already compacted.
func rejectedRecord(t *testing.T, store *requests.Store, id string, tombstone bool) requests.Key {
	t.Helper()
	rec := &requests.Record{
		Snapshot: protocol.Snapshot{
			Schema:      protocol.SchemaRequest,
			RequestID:   id,
			CreatorHost: "local",
			TargetID:    "fake",
			Epoch:       "e_1",
			Revision:    1,
			State:       protocol.StateRejected,
			Code:        protocol.CodeBusy,
			InputDigest: requests.Digest([]byte("hi")),
			ObservedAt:  "2026-09-08T09:00:00Z",
		},
		PublishedRevision: 1,
		Tombstone:         tombstone,
	}
	if err := store.Create(rec); err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
	return requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
}

// TestB14eCompactBudgetCountsCompactedNotScanned reproduces Pro B1 on the
// endpoint sweep: the 100-record budget counts compactions, not records
// scanned. 100 tombstones sort ahead of one compactable record. A budget that
// counted scanned records stopped at the tombstones every tick, so no record
// after them was ever compacted and the store grew without bound.
func TestB14eCompactBudgetCountsCompactedNotScanned(t *testing.T) {
	clk := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	store, err := requests.Open(t.TempDir(), requests.WithClock(func() time.Time { return clk }))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for i := 0; i < 100; i++ {
		rejectedRecord(t, store, fmt.Sprintf("11111111-1111-4111-8111-%012d", i+1), true)
	}
	live := rejectedRecord(t, store, "22222222-2222-4222-8222-222222222201", false)

	ep := core.New(core.Config{
		Store:          store,
		Now:            func() time.Time { return clk },
		Publish:        func(protocol.Snapshot, map[string]string) error { return nil },
		CompactHorizon: time.Minute,
	})
	t.Cleanup(func() { _ = ep.Close() })
	if err := ep.Reconcile(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got, ok, err := store.Get(live)
	if err != nil || !ok {
		t.Fatalf("get live record: %v (ok=%v)", err, ok)
	}
	if !got.Tombstone {
		t.Fatal("record after 100 tombstones was not compacted: the sweep budget counted scanned records (Pro B1)")
	}
}

// TestB14eCompactDoesNotRepublishTombstone pins agent-message-queue-611.22.41:
// CompactOne bumps Revision; the Reconcile publish arm republishes whenever
// PublishedRevision < Revision — so a compaction that left PublishedRevision
// behind made the NEXT Reconcile republish the tombstone (gate repro: a
// publication state=completed code=result_expired result=nil revision=5 for
// an already-delivered result). The tombstone is the record's final published
// state; compaction itself is the publication of the retraction.
func TestB14eCompactDoesNotRepublishTombstone(t *testing.T) {
	// The compaction sweep + a follow-up Reconcile must not
	// emit a second publication for the record. The first publication (the
	// completed result) happens on the initial Reconcile; compaction runs
	// on a later one (shouldCompact rate-limit advances with the clock).
	dir := t.TempDir()
	clk := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	var pubMu sync.Mutex
	var published []protocol.Snapshot
	epStore, err := requests.Open(dir, requests.WithClock(func() time.Time { return clk }))
	if err != nil {
		t.Fatalf("open ep store: %v", err)
	}
	t.Cleanup(func() { _ = epStore.Close() })
	if err := epStore.Create(&requests.Record{
		Snapshot: protocol.Snapshot{
			Schema:      protocol.SchemaRequest,
			RequestID:   "22222222-2222-4222-8222-222222222241",
			CreatorHost: "local",
			TargetID:    "fake",
			Epoch:       "e_1",
			Revision:    1,
			State:       protocol.StateReceived,
			InputDigest: requests.Digest([]byte("hi")),
		},
		Input: &protocol.SubmitInput{Text: "hi"},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	rec, ok, err := epStore.Get(requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: "22222222-2222-4222-8222-222222222241"})
	if err != nil || !ok {
		t.Fatalf("get: %v (ok=%v)", err, ok)
	}
	rec.Revision, rec.State = 2, protocol.StateDispatching
	rec.ObservedAt = "2026-09-08T09:00:00Z"
	if err := epStore.Update(rec); err != nil {
		t.Fatalf("dispatching: %v", err)
	}
	rec.Revision, rec.State = 3, protocol.StateCompleted
	rec.Result = &protocol.Result{Text: "done"}
	rec.AckDigest = protocol.EvidenceDigest(rec.Result) // settled
	if err := epStore.Update(rec); err != nil {
		t.Fatalf("completed: %v", err)
	}
	ep := core.New(core.Config{
		Store: epStore,
		Now:   func() time.Time { return clk },
		Publish: func(s protocol.Snapshot, _ map[string]string) error {
			pubMu.Lock()
			defer pubMu.Unlock()
			published = append(published, s)
			return nil
		},
		CompactHorizon: time.Minute,
	})
	// Reconcile #1: publishes the completed result.
	if err := ep.Reconcile(); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	pubMu.Lock()
	n1 := len(published)
	pubMu.Unlock()
	if n1 == 0 {
		t.Fatal("setup: first reconcile published nothing")
	}
	// Advance past the compaction horizon AND the shouldCompact rate limit,
	// compact via the sweep, then reconcile again: the tombstone must not
	// be republished.
	clk = clk.Add(3 * time.Minute)
	if err := ep.Reconcile(); err != nil {
		t.Fatalf("reconcile 2 (compact): %v", err)
	}
	got, ok, err := epStore.Get(requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: "22222222-2222-4222-8222-222222222241"})
	if err != nil || !ok {
		t.Fatalf("get after sweep: %v (ok=%v)", err, ok)
	}
	if !got.Tombstone {
		t.Fatalf("setup: sweep did not compact (state=%s tombstone=%v)", got.State, got.Tombstone)
	}
	clk = clk.Add(time.Minute) // next shouldCompact window
	if err := ep.Reconcile(); err != nil {
		t.Fatalf("reconcile 3 (post-compact): %v", err)
	}
	pubMu.Lock()
	defer pubMu.Unlock()
	if len(published) != n1 {
		t.Fatalf("tombstone republished: %d publications after compaction (want %d) — agent-message-queue-611.22.41", len(published), n1)
	}
}
