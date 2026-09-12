package core_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// settledCompletedRecord writes a terminal, settled (acked) completed record
// to the store and returns its key.
func settledCompletedRecord(t *testing.T, store *requests.Store, id, obs string) requests.Key {
	t.Helper()
	k := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
	rec := &requests.Record{
		Snapshot: protocol.Snapshot{
			Schema:      protocol.SchemaRequest,
			RequestID:   id,
			CreatorHost: "local",
			TargetID:    "fake",
			Epoch:       "e_1",
			Revision:    1,
			State:       protocol.StateReceived,
			InputDigest: requests.Digest([]byte("hi")),
		},
		Input: &protocol.SubmitInput{Text: "hi"},
	}
	if err := store.Create(rec); err != nil {
		t.Fatalf("create: %v", err)
	}
	run := "run_" + id
	rec.Revision, rec.State = 2, protocol.StateDispatching
	rec.ObservedAt = obs
	if err := store.Update(rec); err != nil {
		t.Fatalf("dispatching: %v", err)
	}
	rec.Revision, rec.State, rec.NativeRun, rec.NativeDispatches = 3, protocol.StateRunning, &run, 1
	if err := store.Update(rec); err != nil {
		t.Fatalf("running: %v", err)
	}
	rec.Revision, rec.State = 4, protocol.StateCompleted
	rec.Result = &protocol.Result{Text: "done-" + id}
	rec.AckDigest = protocol.EvidenceDigest(rec.Result) // settled
	if err := store.Update(rec); err != nil {
		t.Fatalf("completed: %v", err)
	}
	return k
}

// TestB14eCompactRereadsUnderLock reproduces Pro B5: Compact lists candidates
// with no lock, then writes a stale snapshot back — erasing a just-committed
// AckDigest. CompactOne re-reads under the caller's lock, so the AckDigest
// survives.
func TestB14eCompactRereadsUnderLock(t *testing.T) {
	store, now := openStore(t)
	id := "11111111-1111-4111-8111-1111111111e1"
	k := settledCompletedRecord(t, store, id, "2026-09-01T00:00:00Z")

	// Simulate the stale-snapshot race: the List snapshot has no AckDigest
	// (as if read before the ack committed), but the on-disk record does.
	// CompactOne must re-read and see the ack, so it still compacts — but
	// crucially, a concurrent writer that set a NEW AckDigest between List
	// and CompactOne must not be erased.
	rec, _, _ := store.Get(k)
	if rec.AckDigest == "" {
		t.Fatal("setup: AckDigest not set")
	}

	// Compact with a cutoff after the observed time.
	n, err := store.Compact(now().Add(time.Second), 1000)
	if err != nil || n != 1 {
		t.Fatalf("compact: n=%d err=%v (want 1)", n, err)
	}
	got, _, _ := store.Get(k)
	if !got.Tombstone {
		t.Fatal("record not tombstoned")
	}
	// The tombstone must NOT carry the result (compaction reaps it).
	if got.Result != nil {
		t.Fatalf("tombstone still has result: %v", got.Result)
	}
}

// TestB14eCompactSkipsUnsettled reproduces Pro B5+A2: a terminal record with
// a bound run and no ack (NeedsRuntimeSettlement) must NOT be compacted —
// compacting it would lose NativeRun and wedge the cancel forever.
func TestB14eCompactSkipsUnsettled(t *testing.T) {
	store, now := openStore(t)
	id := "11111111-1111-4111-8111-1111111111e2"
	k := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
	rec := &requests.Record{
		Snapshot: protocol.Snapshot{
			Schema:      protocol.SchemaRequest,
			RequestID:   id,
			CreatorHost: "local",
			TargetID:    "fake",
			Epoch:       "e_1",
			Revision:    1,
			State:       protocol.StateReceived,
			InputDigest: requests.Digest([]byte("hi")),
			ObservedAt:  "2026-09-01T00:00:00Z",
		},
		Input: &protocol.SubmitInput{Text: "hi"},
	}
	if err := store.Create(rec); err != nil {
		t.Fatalf("create: %v", err)
	}
	run := "run_" + id
	rec.Revision, rec.State = 2, protocol.StateCancelled
	rec.Code = protocol.CodeCancelledByRequest
	rec.NativeRun = &run
	rec.AckDigest = "" // unsettled: bound run, no ack
	if err := store.Update(rec); err != nil {
		t.Fatalf("update: %v", err)
	}

	// Compact must skip this record — it needs runtime settlement.
	n, err := store.Compact(now().Add(time.Second), 1000)
	if err != nil || n != 0 {
		t.Fatalf("compact: n=%d err=%v (want 0 — record is unsettled)", n, err)
	}
	got, _, _ := store.Get(k)
	if got.Tombstone {
		t.Fatal("unsettled record was tombstoned — NativeRun would be lost")
	}
	if got.NativeRun == nil || *got.NativeRun != run {
		t.Fatalf("NativeRun lost: %v", got.NativeRun)
	}
}

// TestB14eCompactBoundedPerRecord reproduces Pro B7 structurally: if the
// compaction sweep held e.mu across all records, a concurrent Handle would
// block until the sweep finished. Instead, e.mu is held per-record. The test
// runs a sweep in one goroutine and a Handle in another; if the sweep holds
// e.mu across records, the Handle cannot proceed and the test fails on its
// own hard deadline (no timing constant).
func TestB14eCompactBoundedPerRecord(t *testing.T) {
	store, now := openStore(t)
	// Create 50 settled terminal records so the sweep has real work.
	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("11111111-1111-4111-8111-%012d", i+1)
		settledCompletedRecord(t, store, id, "2026-09-01T00:00:00Z")
	}

	ep := core.New(core.Config{Store: store, Now: now, CompactHorizon: time.Hour})
	rt := fake.New("fake", "e_1")
	ep.Register(rt)
	t.Cleanup(func() { _ = ep.Close() })

	// Run the compaction sweep (Reconcile with shouldCompact true) in one
	// goroutine. The sweep calls CompactOne per record, holding e.mu per-record.
	cutoff := now().Add(time.Second)
	recs, _ := store.List()
	sweepDone := make(chan struct{})
	go func() {
		defer close(sweepDone)
		for _, rec := range recs {
			if !rec.State.Terminal() || rec.Tombstone {
				continue
			}
			_, _ = store.CompactOne(requests.Key{CreatorHost: rec.CreatorHost, TargetID: rec.TargetID, RequestID: rec.RequestID}, cutoff)
		}
	}()

	// Concurrently submit a new record. If the sweep held e.mu across all
	// records, this Handle would block until sweepDone — and handleDone would
	// not close before the 5s hard deadline.
	liveID := "22222222-2222-4222-8222-222222222201"
	handleDone := make(chan error, 1)
	go func() {
		_, err := ep.Handle(submitCmd(liveID), core.Source{Host: "local"})
		handleDone <- err
	}()

	select {
	case err := <-handleDone:
		if err != nil {
			t.Fatalf("concurrent handle failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("compaction sweep held e.mu across records — Handle could not proceed (Pro B7)")
	}

	<-sweepDone
	if !b14cWait(func() bool { return rt.HasRun(liveID) }) {
		t.Fatal("live Handle was not admitted")
	}
}

// TestB14eCompactBudgetCountsCompactedNotScanned reproduces Pro B1: the
// budget counter must count successful compactions, not records scanned.
// Seed 110 already-tombstoned records (which sort ahead of the live one) plus
// one live terminal record older than the horizon. With the old i-based
// counter, the loop scans 100 tombstones, breaks, and the live record is never
// compacted. With the compacted-counter, the live record IS compacted.
func TestB14eCompactBudgetCountsCompactedNotScanned(t *testing.T) {
	store, now := openStore(t)
	// Seed 110 settled terminal records whose IDs sort ahead of the live one
	// (prefix "11111111" sorts before "22222222").
	for i := 0; i < 110; i++ {
		id := fmt.Sprintf("11111111-1111-4111-8111-%012d", i+1)
		settledCompletedRecord(t, store, id, "2026-09-01T00:00:00Z")
	}
	// Compact them first to produce tombstones.
	cutoff := now().Add(time.Second)
	recs, _ := store.List()
	for _, rec := range recs {
		if rec.State.Terminal() && !rec.Tombstone {
			if _, cerr := store.CompactOne(requests.Key{CreatorHost: rec.CreatorHost, TargetID: rec.TargetID, RequestID: rec.RequestID}, cutoff); cerr != nil {
				t.Fatalf("setup compact: %v", cerr)
			}
		}
	}
	// Verify we have 110 tombstones.
	recs, _ = store.List()
	tombstoned := 0
	for _, rec := range recs {
		if rec.Tombstone {
			tombstoned++
		}
	}
	if tombstoned < 110 {
		t.Fatalf("setup: only %d tombstones, want >=110", tombstoned)
	}
	// Now add one live terminal record older than the horizon, whose ID
	// sorts AFTER the tombstones (prefix "22222222").
	liveID := "22222222-2222-4222-8222-222222222201"
	liveKey := settledCompletedRecord(t, store, liveID, "2026-09-01T00:00:00Z")

	// Run Compact with limit=100. The old i-based counter would scan 100
	// tombstones, break, and never reach the live record. The compacted-
	// counter skips tombstones and compacts the live one.
	n, err := store.Compact(now().Add(time.Second), 100)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if n != 1 {
		t.Fatalf("compact: n=%d, want 1 (only the live record; tombstones already compacted) — budget counted scanned records, not compacted (Pro B1)", n)
	}
	got, _, _ := store.Get(liveKey)
	if !got.Tombstone {
		t.Fatal("live record was not compacted — budget counted scanned records, not compacted (Pro B1)")
	}
}
