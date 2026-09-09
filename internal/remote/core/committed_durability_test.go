package core_test

import (
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// TestCommittedDurabilityUncertainKeepsRevisionUnpublished pins bead
// 611.22.14 (B10): when publication fails with a committed-durability
// condition — the artifact IS visible at the recipient but its durability is
// indeterminate — the endpoint must treat the publication as done for
// progress purposes (no blind duplicate redelivery), while the record's
// PublishedRevision does NOT advance: the revision was not durably synced,
// so recovery owns the safely-idempotent republish of the same immutable
// revision, not the caller.
func TestCommittedDurabilityUncertainKeepsRevisionUnpublished(t *testing.T) {
	store, _ := openStore(t)
	clk := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	rt := fake.New("fake", "e_1")

	publishCalls := 0
	faultArmed := true
	ep := core.New(core.Config{
		Store: store,
		Now:   func() time.Time { return clk },
		Publish: func(snap protocol.Snapshot, _ map[string]string) error {
			publishCalls++
			if faultArmed {
				return &testCommittedDurabilityError{}
			}
			return nil
		},
	})
	ep.Register(rt)

	id := "11111111-1111-4111-8111-1111111111d1"
	if _, err := ep.Handle(submitCmd(id), core.Source{Host: "local"}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	rt.Complete(id, "the result")
	time.Sleep(50 * time.Millisecond)

	key := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
	rec, ok, err := store.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || rec.State != protocol.StateCompleted {
		t.Fatalf("terminal record not durable: ok=%v state=%v", ok, rec.State)
	}
	if rec.PublishedRevision >= rec.Revision {
		t.Fatalf("PublishedRevision advanced on unsynced delivery: published=%d revision=%d",
			rec.PublishedRevision, rec.Revision)
	}
	if publishCalls != 2 {
		// Submit publishes one revision; the terminal transition publishes the
		// one that hit the committed-durability fault. Both landed BEFORE the
		// faulted publication and must not advance PublishedRevision either.
		t.Fatalf("publish calls before fault = %d, want 2 (submit + terminal)", publishCalls)
	}

	// Reconcile safely republishes the SAME immutable revision once the
	// fault clears — never a new revision, never a duplicate artifact.
	faultArmed = false
	if err := ep.Reconcile(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	rec, _, err = store.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	if rec.PublishedRevision < rec.Revision {
		t.Fatalf("reconcile did not republish the committed revision: published=%d revision=%d",
			rec.PublishedRevision, rec.Revision)
	}
	if publishCalls != 3 {
		t.Fatalf("publish calls = %d, want exactly the original + one safe republish", publishCalls)
	}
}

// testCommittedDurabilityError mirrors fsq.CommittedDurabilityError's shape:
// committed at FinalPath, durability indeterminate, never blindly retried
// with a fresh identifier. (core cannot import fsq — layering.)
type testCommittedDurabilityError struct{}

func (e *testCommittedDurabilityError) Error() string {
	return "committed at /final/path, but durability is indeterminate: injected fault; do not retry blindly"
}
