//go:build darwin || linux

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestPgnChangedStageDoesNotDeadEndLockRemoval verifies that when
// reclaimWakeRestartStateForLockRemovalAt encounters a changed restart-stage
// identity (wakeRestartStageIdentityError), the authoritative lock removal
// does not dead-end: the stage is preserved and the lock is released.
// Bead agent-message-queue-pgn.
//
// This exercises the production code path (removeAuthoritativeWakeClaimAt),
// not a manually constructed error string.
func TestPgnChangedStageDoesNotDeadEndLockRemoval(t *testing.T) {
	fixture := newAuthoritativeWakePreparedCleanupFixture(t)

	// Capture valid image evidence from the test binary so the restart
	// record passes validation.
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	candidate, err := captureWakeImageEvidence(testBinary, "test")
	if err != nil {
		t.Fatalf("captureWakeImageEvidence: %v", err)
	}

	// Write a restart record with schema v1 (no SuccessorGeneration needed)
	// that is NOT bound to the lock generation, so
	// reclaimWakeRestartStateForLockRemovalAt enters the stage-reclaim path
	// without hitting the "live successor handoff" early return.
	requestID, err := newWakeRestartRequestID()
	if err != nil {
		t.Fatalf("newWakeRestartRequestID: %v", err)
	}
	nonBoundGeneration := "00000000000000000000000000000001"
	record := wakeRestartRecord{
		Schema:     wakeRestartSchemaV1,
		RequestID:  requestID,
		Status:     wakeRestartPending,
		Root:       fixture.root,
		Agent:      fixture.me,
		Generation: nonBoundGeneration,
		Owner:      *fixture.inspection.Lock.Owner,
		Candidate:  candidate,
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal restart record: %v", err)
	}
	recordPath := filepath.Join(fixture.agentDir.path, wakeRestartFileName)
	if err := os.WriteFile(recordPath, append(raw, '\n'), 0o600); err != nil {
		t.Fatalf("write restart record: %v", err)
	}

	// Override the platform reclaimer to simulate a changed-stage identity
	// refusal. The lock is live (not stale), so
	// reclaimWakeRestartStateForLockRemovalAt will return the error to the
	// caller (removeAuthoritativeWakeClaimAt).
	original := reclaimWakeRestartStageForStaleLock
	t.Cleanup(func() { reclaimWakeRestartStageForStaleLock = original })
	reclaimWakeRestartStageForStaleLock = func(_ wakeRestartRecord) error {
		return &wakeRestartStageIdentityError{path: "/tmp/fake-stage"}
	}

	// The lock removal must succeed (not dead-end) despite the changed-stage
	// refusal. The stage is preserved; the lock is released.
	if err := fixture.release(); err != nil {
		t.Fatalf("release with changed stage: %v, want nil (stage preserved, lock released)", err)
	}
	fixture.assertReleasedClaimMissing(t)

	// The restart record must be quarantined (not left active) after lock removal.
	if _, err := os.Lstat(filepath.Join(fixture.agentDir.path, wakeRestartFileName)); !os.IsNotExist(err) {
		t.Fatalf("restart record still present after release, want quarantined")
	}

	// A fresh wake claim must be acquirable after the changed-stage release —
	// the preserved stage must not leave the wake state in a dead-end that
	// blocks a new owner from taking over.
	freshLock, err := newWakeLock(fixture.root, fixture.me, wakeLockAcquireOptions{
		target:   &fixture.target,
		wakeMode: wakeTargetInjectVia,
	})
	if err != nil {
		t.Fatalf("fresh acquire after changed-stage release: %v", err)
	}
	if err := withWakeMutationScopeInDir(fixture.agentDir, func(scope *wakeMutationScope) error {
		return publishAuthoritativeWakeClaimAt(scope, fixture.root, fixture.me, fixture.target, freshLock)
	}); err != nil {
		t.Fatalf("publish fresh wake claim after changed-stage release: %v", err)
	}
	t.Cleanup(func() {
		_ = withWakeMutationScopeInDir(fixture.agentDir, func(scope *wakeMutationScope) error {
			freshInspection := inspectWakeLock(fixture.root, fixture.me)
			return removeAuthoritativeWakeClaimAt(scope, freshInspection, &fixture.target)
		})
	})
	freshInspection := inspectWakeLock(fixture.root, fixture.me)
	if classifyPersistedWakeClaim(freshInspection) != wakeClaimAuthoritative {
		t.Fatalf("fresh wake claim = %#v, want authoritative", freshInspection)
	}
}
