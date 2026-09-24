package requests

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// TestReviewCloseCannotReleaseOwnershipBeforeSettlementWrite is the
// agent-message-queue-859 regression (codex owner review 2026-09-24, finding
// 2; reproduction close_review_test.go). A settlement writer that has passed
// its closed check holds the durable-write mutex inside the clock. Close must
// not release the owner lock until that write finishes, or the replacement
// owner's publication marker is overwritten.
func TestReviewCloseCannotReleaseOwnershipBeforeSettlementWrite(t *testing.T) {
	dir := t.TempDir()
	entered, release := make(chan struct{}), make(chan struct{})
	var armed atomic.Bool
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	old, err := Open(dir, WithClock(func() time.Time {
		if armed.CompareAndSwap(true, false) {
			close(entered)
			<-release
		}
		return time.Now()
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = old.Close() }()
	rec := &Record{Snapshot: protocol.Snapshot{
		Schema:      protocol.SchemaRequest,
		RequestID:   "11111111-1111-4111-8111-1111111111c3",
		CreatorHost: "local",
		TargetID:    "fake",
		Epoch:       "e_1",
		Revision:    1,
		State:       protocol.StateReceived,
		InputDigest: Digest([]byte("hi")),
	}, Input: &protocol.SubmitInput{Text: "hi"}}
	if err := old.Create(rec); err != nil {
		t.Fatal(err)
	}
	key := keyOf(rec)
	armed.Store(true)
	writerDone := make(chan error, 1)
	go func() { writerDone <- old.MarkAcknowledged(key) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("writer did not reach clock seam")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- old.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		unblock()
		<-writerDone
		<-closeDone
		return // A serialized Close prevents the ownership gap.
	}
	replacement, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = replacement.Close() }()
	if err := replacement.MarkPublished(key, 1); err != nil {
		t.Fatal(err)
	}
	before, _, err := replacement.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	unblock()
	select {
	case err := <-writerDone:
		if err != nil {
			t.Fatalf("old writer: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("old writer did not finish")
	}
	after, _, err := replacement.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("replacement published before=%d; after stale old writer=%d; old acknowledgement=%v", before.PublishedRevision, after.PublishedRevision, after.Acknowledged)
	if after.PublishedRevision != 1 {
		t.Fatal("closed old owner overwrote replacement owner's committed publication marker")
	}
}
