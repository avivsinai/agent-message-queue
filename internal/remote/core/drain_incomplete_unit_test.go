package core

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// Unit fixture for bead sze (#806 round-6 follow-up): the review claimed
// ErrDrainIncomplete's stateClosed arm was UNREPORTABLE because Close clears
// drainObligations under the lock. Current Close captures the skipped count
// BEFORE clearing and reports after unlock, so the arm must be reachable.
// This fixture proves it end-to-end through the REAL Close: an endpoint with
// an outstanding drain obligation (seeded directly, same technique as the
// b48 marker-only fixture) returns ErrDrainIncomplete from Close even though
// no handler was in flight and the store closes cleanly.
//
// RED if Close ever reverts to clearing the map without reporting (the
// original defect), or if the report is dropped on the clean-store path.
func TestUnitCloseReportsErrDrainIncompleteWithSeededObligation(t *testing.T) {
	clk := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	now := func() time.Time { return clk }
	store, err := requests.Open(t.TempDir(), requests.WithClock(now))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()

	ep := New(Config{Store: store, Now: now})
	defer func() { _ = ep.Close() }() // idempotent: second Close must not re-report

	// Seed the outstanding obligation the way the drain path would leave it:
	// a chained pending revision (r+1) whose publication attempt was skipped.
	key := requests.Key{CreatorHost: "seedhost", RequestID: "req_seed_1"}
	ep.mu.Lock()
	ep.drainObligations[key] = 2 // pendingRev r+1 undischarged
	ep.mu.Unlock()

	err = ep.Close()
	if !errors.Is(err, ErrDrainIncomplete) {
		t.Fatalf("Close err = %v, want ErrDrainIncomplete (stateClosed arm must be reportable)", err)
	}
	if !strings.Contains(err.Error(), "left unpublished") {
		t.Fatalf("Close err does not name the unpublished obligation: %v", err)
	}

	// Close is terminal: a second Close must NOT re-report the cleared
	// obligation (the map was cleared once; the report is exactly-once).
	if err2 := ep.Close(); errors.Is(err2, ErrDrainIncomplete) {
		t.Fatalf("second Close re-reported ErrDrainIncomplete: %v", err2)
	}
}
