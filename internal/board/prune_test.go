package board

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	message := func(name string, ago time.Duration) (Message, []byte) {
		created := time.Now().UTC().Add(-ago)
		id := created.Format("2006-01-02T15-04-05.000Z") + "_pid1_" + name
		raw, _ := json.Marshal(thread.Entry{ID: id, Thread: "work", Kind: "status", Created: created.Format(time.RFC3339Nano)})
		sum := sha256.Sum256(raw)
		var entry thread.Entry
		_ = json.Unmarshal(raw, &entry)
		return MessageFromEntry(base, base, "", entry, hex.EncodeToString(sum[:])), raw
	}
	old, oldRaw := message("old", 3*time.Hour)
	recent, recentRaw := message("recent", 2*time.Hour)
	late, lateRaw := message("late", 4*time.Hour)
	commit := func(msgs []Message, raws [][]byte) {
		t.Helper()
		if err := store.Commit(ctx, Acquisition{ProjectID: initial.ProjectID, Input: Input{Dir: base}, Messages: msgs, Evidence: raws}); err != nil {
			t.Fatal(err)
		}
	}
	commit([]Message{old, recent}, [][]byte{oldRaw, recentRaw})
	if err = store.update(ctx, func(s *Snapshot) error {
		s.PrunedIDs = map[string]time.Time{"ancient": time.Now().Add(-prunedIDHorizon - time.Hour)}
		if !s.pruneHistory(1, nil) {
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

// Review of the disk bounds: pruning dropped an unanswered request, which
// cleanup keeps as an open obligation.
func TestPruneKeepsUnansweredRequests(t *testing.T) {
	now := time.Now().UTC()
	msg := func(name, kind string, ago time.Duration, refs ...string) Message {
		at := now.Add(-ago)
		id := at.Format("2006-01-02T15-04-05.000Z") + "_pid1_" + name
		return Message{Namespace: "ns", Key: "ns#" + id, ID: id, Thread: "work", From: "user", To: []string{"worker"}, Kind: kind, Refs: refs, Created: at.Format(time.RFC3339Nano), EvidenceHash: name}
	}
	ask := msg("ask", "todo", 3*time.Hour)
	note := msg("note", "status", 3*time.Hour, ask.ID)
	chatter := msg("chatter", "status", 2*time.Hour)
	s := Snapshot{Generation: 1, PublishedAt: now, Messages: map[string]Message{ask.Key: ask, note.Key: note, chatter.Key: chatter}}
	if !s.pruneHistory(1<<30, nil) {
		t.Fatal("nothing pruned")
	}
	_, hasAsk := s.Messages[ask.Key]
	_, hasNote := s.Messages[note.Key]
	_, hasChatter := s.Messages[chatter.Key]
	if !hasAsk || !hasNote || hasChatter {
		t.Fatalf("ask=%v note=%v chatter=%v, want the open request and its reference kept", hasAsk, hasNote, hasChatter)
	}
}

// Review of the disk bounds: the evidence of a message skipped as pruned was
// never retired.
func TestSkippedEvidenceIsRetired(t *testing.T) {
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
	message := func(name string, ago time.Duration) (Message, []byte) {
		created := time.Now().UTC().Add(-ago)
		id := created.Format("2006-01-02T15-04-05.000Z") + "_pid1_" + name
		raw, _ := json.Marshal(thread.Entry{ID: id, Thread: "work", Kind: "status", Created: created.Format(time.RFC3339Nano)})
		sum := sha256.Sum256(raw)
		var entry thread.Entry
		_ = json.Unmarshal(raw, &entry)
		return MessageFromEntry(base, base, "", entry, hex.EncodeToString(sum[:])), raw
	}
	commit := func(m Message, raw []byte) {
		t.Helper()
		err := store.Commit(ctx, Acquisition{ProjectID: initial.ProjectID, Input: Input{Dir: base}, Messages: []Message{m}, Evidence: [][]byte{raw}})
		if err != nil && !errors.Is(err, errEvidenceBound) {
			t.Fatal(err)
		}
	}
	read := func() Snapshot {
		t.Helper()
		got, err := store.Read()
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	skipped, skippedRaw := message("skipped", 2*time.Hour)
	if err = store.update(ctx, func(s *Snapshot) error {
		s.PrunedIDs = map[string]time.Time{skipped.ID: time.Now()}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	commit(skipped, skippedRaw)
	if _, ok := read().RetiredEvidence[skipped.EvidenceHash]; !ok {
		t.Fatal("evidence of a message skipped as pruned is not retired")
	}
}

// Review of the disk bounds, round 2, finding 3: evidence that nothing names
// counted against the evidence bound, so unheld history was pruned for it.
// A bound passed by named evidence alone needs 256 MiB of real history, too
// large for a fast test.
func TestUnnamedEvidenceAtTheBoundPrunesNoHistory(t *testing.T) {
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
	message := func(name string, ago time.Duration) (Message, []byte) {
		created := time.Now().UTC().Add(-ago)
		id := created.Format("2006-01-02T15-04-05.000Z") + "_pid1_" + name
		raw, _ := json.Marshal(thread.Entry{ID: id, Thread: "work", Kind: "status", Created: created.Format(time.RFC3339Nano)})
		sum := sha256.Sum256(raw)
		var entry thread.Entry
		_ = json.Unmarshal(raw, &entry)
		return MessageFromEntry(base, base, "", entry, hex.EncodeToString(sum[:])), raw
	}
	commit := func(m Message, raw []byte) {
		t.Helper()
		err := store.Commit(ctx, Acquisition{ProjectID: initial.ProjectID, Input: Input{Dir: base}, Messages: []Message{m}, Evidence: [][]byte{raw}})
		if err != nil && !errors.Is(err, errEvidenceBound) {
			t.Fatal(err)
		}
	}
	kept, keptRaw := message("kept", 2*time.Hour)
	commit(kept, keptRaw)
	orphan := filepath.Join(store.Dir, "evidence", strings.Repeat("0", 64)+".json")
	if err = os.WriteFile(orphan, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.Truncate(orphan, evidenceLimit); err != nil {
		t.Fatal(err)
	}
	next, nextRaw := message("next", time.Hour)
	commit(next, nextRaw)
	got, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("unnamed evidence is still on disk: %v", err)
	}
	if _, ok := got.Messages[next.Key]; !ok {
		t.Fatal("the refresh did not ingest after the unnamed evidence went")
	}
	if _, ok := got.Messages[kept.Key]; !ok || len(got.RetiredEvidence) != 0 {
		t.Fatalf("history was pruned for unnamed evidence: kept=%v retired=%v", ok, got.RetiredEvidence)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, "evidence", next.EvidenceHash+".json")); err != nil {
		t.Fatalf("named evidence was removed: %v", err)
	}
}

// Review of the disk bounds, round 2, finding 2: evidence staged for a
// refresh and removed by a new base before publication read as a store that
// needs recovery. A message with no staged evidence stands for that case.
func TestCommitWithSweptEvidenceIsRetryable(t *testing.T) {
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
	id := time.Now().UTC().Format("2006-01-02T15-04-05.000Z") + "_pid1_a"
	raw, _ := json.Marshal(thread.Entry{ID: id, Thread: "work", Kind: "status", Created: time.Now().UTC().Format(time.RFC3339Nano)})
	sum := sha256.Sum256(raw)
	var entry thread.Entry
	_ = json.Unmarshal(raw, &entry)
	m := MessageFromEntry(base, base, "", entry, hex.EncodeToString(sum[:]))
	err = store.Commit(ctx, Acquisition{ProjectID: initial.ProjectID, Input: Input{Dir: base}, Messages: []Message{m}})
	if err == nil || strings.Contains(err.Error(), "recovery required") {
		t.Fatalf("commit = %v, want a retryable refresh error", err)
	}
	if _, err = store.Read(); err != nil {
		t.Fatalf("store after the refresh: %v", err)
	}
}
