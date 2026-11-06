package board

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/thread"
)

// A full board prunes its oldest unheld history instead of stopping
// ingestion and does not take that history back, while a late message
// created before the pruned ones still arrives, and pruned ids past the
// horizon are forgotten (board ingestion stopping at its cap, bead
// agent-message-queue-9df; late delivery dropped by a creation time floor
// and an unbounded pruned id set, reviews of that bead).
func TestPruneDropsOldestUnheldHistoryAndKeepsLateMail(t *testing.T) {
	ctx := context.Background()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	identity, err := fsq.StableTreeIdentity(base)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(base, identity, "host-test")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	initial, err := store.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	message := func(id, created string) (Message, []byte) {
		raw, _ := json.Marshal(thread.Entry{ID: id, Thread: "work", Kind: "status", Created: created})
		sum := sha256.Sum256(raw)
		var entry thread.Entry
		_ = json.Unmarshal(raw, &entry)
		return MessageFromEntry(base, base, "", entry, hex.EncodeToString(sum[:])), raw
	}
	at := func(ago time.Duration) string { return time.Now().UTC().Add(-ago).Format(time.RFC3339Nano) }
	old, oldRaw := message("old", at(3*time.Hour))
	recent, recentRaw := message("recent", at(2*time.Hour))
	late, lateRaw := message("late", at(4*time.Hour))
	commit := func(msgs []Message, raws [][]byte) {
		t.Helper()
		if err := store.Commit(ctx, Acquisition{ProjectID: initial.ProjectID, Input: Input{Dir: base}, Messages: msgs, Evidence: raws}); err != nil {
			t.Fatal(err)
		}
	}
	commit([]Message{old, recent}, [][]byte{oldRaw, recentRaw})
	if err = store.update(ctx, func(s *Snapshot) error {
		s.PrunedIDs = map[string]time.Time{"ancient": time.Now().Add(-prunedIDHorizon - time.Hour)}
		if !s.pruneHistory(1) {
			t.Fatal("nothing pruned")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	commit([]Message{old, recent, late}, [][]byte{oldRaw, recentRaw, lateRaw})
	got, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	_, hasOld := got.Messages[old.Key]
	_, hasRecent := got.Messages[recent.Key]
	_, hasLate := got.Messages[late.Key]
	if hasOld || !hasRecent || !hasLate {
		t.Fatalf("old=%v recent=%v late=%v, want only the pruned message gone", hasOld, hasRecent, hasLate)
	}
	if _, ok := got.PrunedIDs["ancient"]; ok || len(got.PrunedIDs) != 1 {
		t.Fatalf("pruned ids = %v, want only the recent prune", got.PrunedIDs)
	}
}
