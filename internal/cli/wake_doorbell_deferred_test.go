package cli

import (
	"errors"
	"os"
	"testing"
	"time"
)

// A deferred injector outcome recorded while the doorbell is ANNOUNCED (the
// interrupt path runs before plan(), so plan()'s expansion re-arm never
// fires) must re-arm the ladder for unseen additions. A no-reconcile
// implementation leaves phase=announced, and nextDeadline() returns nothing —
// the deferred doorbell stalls until an unrelated inbox event (#708).
func TestDeferredAttemptReArmsAnnouncedCohortOnAddition(t *testing.T) {
	now := time.Now()
	seen := fakeWakePendingFiles(t, "a.md")
	cfg := &wakeConfig{retryUntil: wakeRetryUntilInjected, injectVia: "/bin/echo"}
	cfg.doorbell.arm(seen)
	cfg.doorbell.recordInjected(seen)
	if cfg.doorbell.phase != wakeDoorbellAnnounced {
		t.Fatalf("setup phase = %v, want announced", cfg.doorbell.phase)
	}

	expanded := fakeWakePendingFiles(t, "a.md", "b.md")
	deferredErr := &wakeInjectorDeferredError{err: errors.New("provider busy")}
	recordWakeAttempt(cfg, now, expanded, deferredErr)

	if cfg.doorbell.phase != wakeDoorbellRetrying {
		t.Fatalf("phase after deferred addition = %v, want retrying", cfg.doorbell.phase)
	}
	if _, ok := cfg.doorbell.cohort["b.md"]; !ok {
		t.Fatal("deferred re-arm did not adopt the added message into the cohort")
	}
	if _, armed := cfg.doorbell.nextDeadline(); !armed {
		t.Fatal("deferred attempt on an announced cohort left no retry deadline (silent stall)")
	}
}

// Same for a PARKED cohort: an addition must revive the ladder (with the
// lifetime-budget increment plan() uses); an unchanged parked cohort must
// stay parked — deferred is not an excuse to resurrect a terminally parked
// cohort with no new content.
func TestDeferredAttemptRevivesParkedCohortOnlyOnAddition(t *testing.T) {
	now := time.Now()
	seen := fakeWakePendingFiles(t, "a.md")
	cfg := &wakeConfig{retryUntil: wakeRetryUntilInjected, injectVia: "/bin/echo"}
	cfg.doorbell.arm(seen)
	cfg.doorbell.parkCurrentCohort()

	deferredErr := &wakeInjectorDeferredError{err: errors.New("provider busy")}

	// Unchanged cohort: stays parked, no deadline.
	recordWakeAttempt(cfg, now, seen, deferredErr)
	if cfg.doorbell.phase != wakeDoorbellParked {
		t.Fatalf("unchanged parked cohort phase = %v, want parked", cfg.doorbell.phase)
	}

	// Addition: revives.
	expanded := fakeWakePendingFiles(t, "a.md", "b.md")
	recordWakeAttempt(cfg, now, expanded, deferredErr)
	if cfg.doorbell.phase != wakeDoorbellRetrying {
		t.Fatalf("parked+addition phase = %v, want retrying", cfg.doorbell.phase)
	}
	if _, armed := cfg.doorbell.nextDeadline(); !armed {
		t.Fatal("parked+addition deferred attempt left no retry deadline")
	}
}

func fakeWakePendingFiles(t *testing.T, names ...string) map[string]os.FileInfo {
	t.Helper()
	dir := t.TempDir()
	out := make(map[string]os.FileInfo, len(names))
	for _, name := range names {
		path := dir + "/" + name
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		out[name] = info
	}
	return out
}
