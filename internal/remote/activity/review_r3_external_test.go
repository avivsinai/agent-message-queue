package activity

import (
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
)

// Codex #865 r3 2026-09-23T07-27-24.009Z_pid72141_c6b5eb5c: equal block
// ceilings must not reuse the persisted reservation.
func TestReviewR3UpgradeReadsEqualDurableHighWater(t *testing.T) {
	dir := t.TempDir()
	var old seqAllocator
	_, err := old.commit("body/native", dir)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := old.commit("body/native", dir)
	if err != nil {
		t.Fatal(err)
	}
	var current seqAllocator
	_, err = current.commit("body/native", "")
	if err != nil {
		t.Fatal(err)
	}
	upgraded, err := current.commit("body/native", dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("old process issued=%d, durable reservation=1024, upgraded issued=%d", issued, upgraded)
	if upgraded <= 1024 {
		t.Fatalf("upgrade reused prior durable reservation: %d", upgraded)
	}
}

// Codex #865 r3 2026-09-23T07-27-24.009Z_pid72141_c6b5eb5c: Close must not
// leave ciphertext or the sink pointer in the global body map.
func TestReviewR3CloseReleasesRetainedFrames(t *testing.T) {
	now := time.Now()
	sink := &Sink{Now: func() time.Time { return now }}
	var q liveQueue
	q.push(sink, "body", nostr.Event{CreatedAt: nostr.Timestamp(now.Unix()), Content: strings.Repeat("x", 64000)}, 64000)
	q.release(sink, "body")
	retained := q.body["body"]
	if len(retained) != 0 || q.processBytes != 0 {
		t.Fatal("close did not clear accounting")
	}
	if cap(retained) > 0 && retained[:cap(retained)][0].evt.Content != "" {
		t.Fatalf("close reports zero queued bytes but global map retains %d bytes and sink pointer", len(retained[:cap(retained)][0].evt.Content))
	}
}
