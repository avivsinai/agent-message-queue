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

// PR #1008 review: an urgent interrupt's deferred control injection runs before
// planHeld. The capped revival consumed its identity while leaving the cohort
// parked, so the unseen urgent message had no retry deadline. One new urgent
// message grants one reminder; deferral does not spend it or grant more.
func TestDeferredUrgentAttemptReArmsExhaustedParkedCohort(t *testing.T) {
	now := time.Now()
	seen := wakeDoorbellTestFiles(t, "old.md")
	cfg := &wakeConfig{}
	cfg.doorbell.arm(seen)
	cfg.doorbell.attemptBudget = wakeDoorbellLifetimeAttemptCap
	for i := uint(0); i < wakeDoorbellLifetimeAttemptCap; i++ {
		cfg.doorbell.recordAttempt(now)
	}
	expanded := withAddedWakeFile(t, seen, "urgent.md")
	cfg.holds = map[string]wakeHold{"urgent.md": {due: now, urgent: true}}
	recordWakeAttempt(cfg, now, expanded, deferredInjectorErrorForTest())
	deadline, ok := cfg.doorbell.nextDeadline()
	if !ok {
		t.Fatal("deferred urgent addition has no retry deadline")
	}
	if got := cfg.doorbell.attemptBudget; got != wakeDoorbellLifetimeAttemptCap+1 {
		t.Fatalf("attempt budget = %d, want exactly one new attempt", got)
	}
	// Seeing the same deferred urgent again must keep the same budget.
	recordWakeAttempt(cfg, deadline, expanded, deferredInjectorErrorForTest())
	deadline, ok = cfg.doorbell.nextDeadline()
	if !ok {
		t.Fatal("repeated deferral lost the retry deadline")
	}
	if got := cfg.doorbell.attemptBudget; got != wakeDoorbellLifetimeAttemptCap+1 {
		t.Fatalf("repeated deferral raised attempt budget to %d", got)
	}
	if plan := cfg.doorbell.planHeld(deadline, expanded, cfg.holds); !plan.attempt {
		t.Fatal("urgent retry was not due at its deadline")
	}
	recordWakeAttempt(cfg, deadline, expanded, nil)
	if cfg.doorbell.phase != wakeDoorbellParked {
		t.Fatal("successful urgent retry did not park the spent cohort")
	}
	if plan := cfg.doorbell.planHeld(deadline.Add(wakeDoorbellRetryMax), expanded, cfg.holds); plan.attempt {
		t.Fatal("same urgent message revived the parked cohort again")
	}
}
