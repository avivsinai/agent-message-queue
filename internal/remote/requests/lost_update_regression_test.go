package requests

import (
	"sync"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// TestMarkerWritesDoNotLoseUpdates pins the 611.22.34 B3 store-level fix:
// MarkAcknowledged and MarkPublished are field-level merges on a FRESH read
// under the store write lock. With raw Get->mutate->write (the shape this PR
// first shipped) one marker silently reverts the other: losing
// PublishedRevision republishes a delivered revision; losing Acknowledged
// puts the record back on the non-converging replay path. Deterministic:
// 200/200 losses on the reverted shape (verifier probe, three runs).
func TestMarkerWritesDoNotLoseUpdates(t *testing.T) {
	for round := 0; round < 200; round++ {
		s, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		id := "11111111-1111-4111-8111-1111111111c3"
		rec := &Record{Snapshot: protocol.Snapshot{
			Schema:      protocol.SchemaRequest,
			RequestID:   id,
			CreatorHost: "local",
			TargetID:    "fake",
			Epoch:       "e_1",
			Revision:    1,
			State:       protocol.StateReceived,
			InputDigest: Digest([]byte("hi")),
		}, Input: &protocol.SubmitInput{Text: "hi"}}
		if err := s.Create(rec); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = s.MarkAcknowledged(keyOf(rec))
		}()
		go func() {
			defer wg.Done()
			_ = s.MarkPublished(keyOf(rec), 1)
		}()
		wg.Wait()
		got, ok, err := s.Get(keyOf(rec))
		if err != nil || !ok {
			t.Fatalf("round %d: get %v ok=%v", round, err, ok)
		}
		if !got.Acknowledged || got.PublishedRevision != 1 {
			t.Fatalf("round %d: LOST UPDATE — acknowledged=%v published_revision=%d (want true/1): one marker silently reverted the other (agent-message-queue-611.22.34 B3)", round, got.Acknowledged, got.PublishedRevision)
		}
		_ = s.Close()
	}
}
