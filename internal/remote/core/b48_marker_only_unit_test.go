package core

import (
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// Unit fixture (codex 12:07 verdict + 16:43 completion criterion): the
// round-6 full-schedule test could not create the drainObligations
// precondition, so Close saw an empty map regardless of marker-only
// retirement. This same-package unit fixture seeds the observed intermediate
// state directly — an outstanding obligation for key, a confirmed delivery
// awaiting its marker (visible[key] == revision, PublishedRevision <
// Revision) — and exercises the REAL publishLocked marker-only branch, not
// the retirement helper in isolation.
//
// What it proves:
//  1. With a seeded outstanding obligation and a delivered-but-unmarked
//     revision, publishLocked takes the marker-only retry path: the
//     delivery is skipped (no second publish call) and MarkPublished
//     advances PublishedRevision.
//  2. The marker-only path RETIRES the obligation. Deleting the
//     retireObligationLocked call from the marker-only branch makes the
//     obligation assertion fail.
//  3. Close therefore returns nil instead of ErrDrainIncomplete.
//
// Labeled honestly: a unit fixture seeding an internal state, not live
// proof through the full native schedule.
func TestUnitMarkerOnlyRetryRetiresSeededObligation(t *testing.T) {
	clk := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	now := func() time.Time { return clk }
	store, err := requests.Open(t.TempDir(), requests.WithClock(now))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()

	var pubCalls int
	pub := func(s protocol.Snapshot, origin map[string]string) error {
		pubCalls++
		return nil
	}
	ep := New(Config{Store: store, Publish: pub, Now: now})

	id := "11111111-1111-4111-8111-111111111491"
	key := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}

	// Seed a record at revision 2 (the question revision): delivered
	// (visible set) but NOT marked published, plus an outstanding drain
	// obligation for this key at revision 2 — exactly the state the
	// obligation records when a chained attempt is skipped.
	rec := seedRecord(t, store, id, 2)

	// Exercise the real publishLocked marker-only branch under the
	// endpoint's lock (same entry publishLocked's callers use).
	ep.mu.Lock()
	ep.visible[key] = rec.Revision
	ep.drainObligations[key] = rec.Revision
	ep.publishLocked(rec)
	_, markedOK, gerr := store.Get(key)
	ep.mu.Unlock()
	if gerr != nil || !markedOK {
		t.Fatalf("seed record missing after publishLocked: ok=%v err=%v", markedOK, gerr)
	}
	cur, _, _ := store.Get(key)
	if cur.PublishedRevision < cur.Revision {
		t.Fatalf("marker-only retry did not advance PublishedRevision: %d < %d", cur.PublishedRevision, cur.Revision)
	}
	if pubCalls != 0 {
		t.Fatalf("marker-only retry delivered again: publish calls=%d, want 0 (delivery must be skipped)", pubCalls)
	}

	// The obligation must be retired by the marker-only path.
	ep.mu.Lock()
	ob, hasOb := ep.drainObligations[key]
	ep.mu.Unlock()
	if hasOb {
		t.Fatalf("obligation for key still outstanding at revision %d after marker-only retirement", ob)
	}

	// Close returns nil: nothing is left for ErrDrainIncomplete.
	if err := ep.Close(); err != nil {
		t.Fatalf("close returned error after marker-only retirement; want nil: %v", err)
	}
}

// seedRecord persists a record for id at the given revision with
// PublishedRevision left below Revision (delivered-but-unmarked).
func seedRecord(t *testing.T, store *requests.Store, id string, rev int64) *requests.Record {
	t.Helper()
	rec := &requests.Record{
		Input: &protocol.SubmitInput{Text: "x"},
		Snapshot: protocol.Snapshot{
			Schema:      protocol.SchemaRequest,
			RequestID:   id,
			CreatorHost: "local",
			TargetID:    "fake",
			Epoch:       "e_1",
			State:       protocol.StateReceived,
			Revision:    1,
		},
	}
	if err := store.Create(rec); err != nil {
		t.Fatalf("seed record rev 1: %v", err)
	}
	// Walk the record up to the target revision through the store's own
	// state graph (received -> dispatching -> running), leaving
	// PublishedRevision below Revision: the delivered-but-unmarked state.
	for r := int64(2); r <= rev; r++ {
		next := *rec
		next.Revision = r
		switch {
		case r < rev:
			next.State = protocol.StateDispatching
		case rec.State == protocol.StateDispatching:
			next.State = protocol.StateRunning
		default:
			next.State = protocol.StateDispatching
		}
		if err := store.Update(&next); err != nil {
			t.Fatalf("seed record rev %d: %v", r, err)
		}
		*rec = next
	}
	// Land on Running for the final revision if the walk ended elsewhere
	// (an extra dispatching hop keeps the graph valid).
	if rev > 1 && rec.State != protocol.StateRunning {
		final := *rec
		final.Revision = rev + 1
		final.State = protocol.StateRunning
		if err := store.Update(&final); err != nil {
			t.Fatalf("seed record final: %v", err)
		}
		*rec = final
	}
	return rec
}
