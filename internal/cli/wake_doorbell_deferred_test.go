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

// withoutWakeFile returns a copy of cohort with one file drained.
func withoutWakeFile(cohort map[string]os.FileInfo, name string) map[string]os.FileInfo {
	out := make(map[string]os.FileInfo, len(cohort))
	for k, v := range cohort {
		if k != name {
			out[k] = v
		}
	}
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

// An in-place replacement (same name, different physical file) is a message
// the agent has never seen. plan() re-arms an announced cohort for it; the
// deferred interrupt path must too. An expansion-only reconcile leaves the
// replaced message announced with no deadline.
func TestDeferredAttemptReArmsAnnouncedCohortOnInPlaceReplacement(t *testing.T) {
	now := time.Now()
	seen := wakeDoorbellTestFiles(t, "a.md")
	cfg := &wakeConfig{retryUntil: wakeRetryUntilInjected, injectVia: "/bin/echo"}
	cfg.doorbell.arm(seen)
	cfg.doorbell.recordInjected(seen)

	replaced := wakeDoorbellTestFiles(t, "a.md") // fresh temp dir: new inode, same name
	if !wakeCohortReplacedInPlace(cfg.doorbell.cohort, replaced) {
		t.Skip("filesystem does not expose a distinct identity for the replacement")
	}
	recordWakeAttempt(cfg, now, replaced, deferredInjectorErrorForTest())

	if cfg.doorbell.phase != wakeDoorbellRetrying {
		t.Fatalf("phase after deferred replacement = %v, want retrying", cfg.doorbell.phase)
	}
	if _, armed := cfg.doorbell.nextDeadline(); !armed {
		t.Fatal("deferred attempt on a replaced announced message left no retry deadline")
	}
}

// Progress on an announced cohort (the agent drained part of it) is not new
// content: the cohort re-snapshots to what remains (plan()'s recordInjected
// rule) and stays announced. A reconcile that arms on any change would spam a
// doorbell for messages the agent has already seen.
func TestDeferredAttemptOnAnnouncedProgressReSnapshotsWithoutReArm(t *testing.T) {
	now := time.Now()
	seen := wakeDoorbellTestFiles(t, "a.md", "b.md")
	cfg := &wakeConfig{retryUntil: wakeRetryUntilInjected, injectVia: "/bin/echo"}
	cfg.doorbell.arm(seen)
	cfg.doorbell.recordInjected(seen)

	drained := withoutWakeFile(seen, "b.md")
	recordWakeAttempt(cfg, now, drained, deferredInjectorErrorForTest())

	if cfg.doorbell.phase != wakeDoorbellAnnounced {
		t.Fatalf("phase after deferred progress = %v, want announced (no new content)", cfg.doorbell.phase)
	}
	if len(cfg.doorbell.cohort) != 1 {
		t.Fatalf("announced cohort after progress = %d files, want 1", len(cfg.doorbell.cohort))
	}
	if _, stale := cfg.doorbell.cohort["b.md"]; stale {
		t.Fatal("drained message still in the announced cohort snapshot")
	}
}

// Same for a PARKED cohort: an addition must revive the ladder (with the
// lifetime-budget increment plan() uses); an unchanged parked cohort must
// stay parked — deferred is not an excuse to resurrect a terminally parked
// cohort with no new content.
func TestDeferredAttemptRevivesParkedCohortOnlyOnAddition(t *testing.T) {
	now := time.Now()
	seen := wakeDoorbellTestFiles(t, "a.md")
	cfg := &wakeConfig{retryUntil: wakeRetryUntilInjected, injectVia: "/bin/echo"}
	cfg.doorbell.arm(seen)
	cfg.doorbell.parkCurrentCohort()
	budget := cfg.doorbell.attemptBudget

	// Unchanged cohort: stays parked, no deadline.
	recordWakeAttempt(cfg, now, seen, deferredInjectorErrorForTest())
	if cfg.doorbell.phase != wakeDoorbellParked {
		t.Fatalf("unchanged parked cohort phase = %v, want parked", cfg.doorbell.phase)
	}
	if _, armed := cfg.doorbell.nextDeadline(); armed {
		t.Fatal("unchanged parked cohort must not regain a retry deadline")
	}

	// Addition: revives, spending one lifetime-budget unit.
	expanded := withAddedWakeFile(t, seen, "b.md")
	recordWakeAttempt(cfg, now, expanded, deferredInjectorErrorForTest())
	if cfg.doorbell.phase != wakeDoorbellRetrying {
		t.Fatalf("parked+addition phase = %v, want retrying", cfg.doorbell.phase)
	}
	if cfg.doorbell.attemptBudget != budget+1 {
		t.Fatalf("parked+addition budget = %d, want %d (plan()'s revive increment)", cfg.doorbell.attemptBudget, budget+1)
	}
	if _, armed := cfg.doorbell.nextDeadline(); !armed {
		t.Fatal("parked+addition deferred attempt left no retry deadline")
	}
}

// The lifetime cap is the anti-spam invariant: a parked cohort that has
// already spent wakeDoorbellLifetimeAttemptCap must NOT revive on a further
// addition. An implementation that revives unconditionally passes the
// addition test above and fails here.
func TestDeferredAttemptDoesNotReviveParkedCohortPastLifetimeCap(t *testing.T) {
	now := time.Now()
	seen := wakeDoorbellTestFiles(t, "a.md")
	cfg := &wakeConfig{retryUntil: wakeRetryUntilInjected, injectVia: "/bin/echo"}
	cfg.doorbell.arm(seen)
	cfg.doorbell.attemptBudget = wakeDoorbellLifetimeAttemptCap
	cfg.doorbell.parkCurrentCohort()

	expanded := withAddedWakeFile(t, seen, "b.md")
	recordWakeAttempt(cfg, now, expanded, deferredInjectorErrorForTest())

	if cfg.doorbell.phase != wakeDoorbellParked {
		t.Fatalf("parked cohort at lifetime cap phase = %v, want parked", cfg.doorbell.phase)
	}
	if cfg.doorbell.attemptBudget != wakeDoorbellLifetimeAttemptCap {
		t.Fatalf("budget past cap = %d, want %d", cfg.doorbell.attemptBudget, wakeDoorbellLifetimeAttemptCap)
	}
	if _, armed := cfg.doorbell.nextDeadline(); armed {
		t.Fatal("parked cohort at lifetime cap regained a retry deadline (anti-spam cap broken)")
	}
}

// Progress on a PARKED cohort (agent drained one message, an urgent one
// remains, the interrupt injector defers) follows plan()'s rule: reset the
// ladder and arm the remaining cohort fresh. An expansion-only reconcile
// leaves it parked, nextDeadline() returns nothing, and the deferred urgent
// interrupt stalls until an unrelated inbox event.
func TestDeferredAttemptOnParkedProgressResetsAndArms(t *testing.T) {
	now := time.Now()
	seen := wakeDoorbellTestFiles(t, "a.md", "urgent.md")
	cfg := &wakeConfig{retryUntil: wakeRetryUntilInjected, injectVia: "/bin/echo"}
	cfg.doorbell.arm(seen)
	cfg.doorbell.parkCurrentCohort()

	drained := withoutWakeFile(seen, "a.md")
	recordWakeAttempt(cfg, now, drained, deferredInjectorErrorForTest())

	if cfg.doorbell.phase != wakeDoorbellRetrying {
		t.Fatalf("parked+progress phase = %v, want retrying", cfg.doorbell.phase)
	}
	if cfg.doorbell.attemptBudget != wakeDoorbellInitialAttemptBudget {
		t.Fatalf("parked+progress budget = %d, want fresh initial budget %d", cfg.doorbell.attemptBudget, wakeDoorbellInitialAttemptBudget)
	}
	if len(cfg.doorbell.cohort) != 1 {
		t.Fatalf("armed cohort after progress = %d files, want 1", len(cfg.doorbell.cohort))
	}
	if _, armed := cfg.doorbell.nextDeadline(); !armed {
		t.Fatal("parked+progress deferred attempt left no retry deadline (urgent interrupt stalls)")
	}
}
