package cli

import (
	"errors"
	"os"
	"testing"
	"time"
)

func deferredInjectorErrorForTest() error {
	return &wakeInjectorDeferredError{err: errors.New("provider busy")}
}

// withAddedWakeFile returns cohort plus one new file, keeping the existing
// entries' physical identities (a fresh wakeDoorbellTestFiles call would give
// every name a new inode and read as an in-place replacement, not an addition).
func withAddedWakeFile(t *testing.T, cohort map[string]os.FileInfo, name string) map[string]os.FileInfo {
	t.Helper()
	out := make(map[string]os.FileInfo, len(cohort)+1)
	for k, v := range cohort {
		out[k] = v
	}
	out[name] = wakeDoorbellTestFiles(t, name)[name]
	return out
}

// A deferred injector outcome recorded while the doorbell is ANNOUNCED (the
// interrupt path runs before plan(), so plan()'s expansion re-arm never
// fires) must re-arm the ladder for unseen additions. A no-reconcile
// implementation leaves phase=announced, and nextDeadline() returns nothing —
// the deferred doorbell stalls until an unrelated inbox event (#708).
func TestDeferredAttemptReArmsAnnouncedCohortOnAddition(t *testing.T) {
	now := time.Now()
	seen := wakeDoorbellTestFiles(t, "a.md")
	cfg := &wakeConfig{retryUntil: wakeRetryUntilInjected, injectVia: "/bin/echo"}
	cfg.doorbell.arm(seen)
	cfg.doorbell.recordInjected(seen)
	if cfg.doorbell.phase != wakeDoorbellAnnounced {
		t.Fatalf("setup phase = %v, want announced", cfg.doorbell.phase)
	}

	expanded := withAddedWakeFile(t, seen, "b.md")
	recordWakeAttempt(cfg, now, expanded, deferredInjectorErrorForTest())

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
