package requests

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// TestSweepReadsOnlyRecordsThatNeedWork is agent-message-queue-9dx.6: with
// 5,000 settled records and 3 that owe work on disk, Open rebuilds the
// index from the files and the sweep reads only the 3; a write that settles
// one takes it out. A tombstone that an older binary compacted with its
// retired outcomes still set owes nothing (ruling ss).
func TestSweepReadsOnlyRecordsThatNeedWork(t *testing.T) {
	dir := t.TempDir()
	recDir := filepath.Join(dir, layoutVersion, requestsDir, "hostA")
	if err := os.MkdirAll(recDir, dirMode); err != nil {
		t.Fatal(err)
	}
	// Written as plain files, as a previous process left them: Open must
	// rebuild the index from the records alone.
	put := func(rec *Record) {
		rec.UpdatedAt = protocol.FormatTime(fixedClock())
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		name := rec.TargetID + "__" + rec.RequestID + recordSuffix
		if err := os.WriteFile(filepath.Join(recDir, name), data, fileMode); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 5000 {
		rec := newRecord(fmt.Sprintf("11111111-1111-4111-8111-%012d", i))
		rec.State, rec.Code, rec.PublishedRevision = protocol.StateRejected, protocol.CodeBusy, 1
		put(rec)
	}
	oldTombstone := newRecord("33333333-3333-4333-8333-000000000001")
	oldTombstone.Revision, oldTombstone.State, oldTombstone.PublishedRevision = 3, protocol.StateCompleted, 3
	oldTombstone.Code, oldTombstone.Tombstone, oldTombstone.Input = protocol.CodeResultExpired, true, nil
	oldTombstone.RetiredOutcomes = []string{"i_1"}
	put(oldTombstone)
	running := newRecord("22222222-2222-4222-8222-000000000001")
	running.Revision, running.State, running.PublishedRevision = 3, protocol.StateRunning, 3
	put(running)
	unpublished := newRecord("22222222-2222-4222-8222-000000000002")
	unpublished.Revision, unpublished.State, unpublished.PublishedRevision = 2, protocol.StateRejected, 1
	put(unpublished)
	unacked := newRecord("22222222-2222-4222-8222-000000000003")
	unacked.Revision, unacked.State, unacked.PublishedRevision = 4, protocol.StateCompleted, 4
	unacked.Result = &protocol.Result{Text: "hi"}
	unacked.AckDigest = protocol.EvidenceDigest(unacked.Result)
	put(unacked)

	s, err := Open(dir, WithClock(fixedClock))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()

	sweepIDs := func() map[string]bool {
		ids := map[string]bool{}
		for _, rec := range s.ListSweep() {
			ids[rec.RequestID] = true
		}
		return ids
	}
	if got := sweepIDs(); len(got) != 3 || !got[running.RequestID] || !got[unpublished.RequestID] || !got[unacked.RequestID] {
		t.Fatalf("sweep read %d records %v, want the 3 that owe work", len(got), got)
	}

	if err := s.MarkPublished(keyOf(unpublished), 2); err != nil {
		t.Fatalf("mark published: %v", err)
	}
	if got := sweepIDs(); len(got) != 2 || got[unpublished.RequestID] {
		t.Fatalf("after publication the sweep reads %v, want running and unacked only", got)
	}
}
