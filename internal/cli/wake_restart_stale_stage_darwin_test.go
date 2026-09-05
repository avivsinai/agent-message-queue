//go:build darwin

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Regression: the 2026-09-05 reboot report had a dead wake and an unchanged
// staged executable whose device number differed from the persisted evidence.
func TestStaleWakeReleasePreservesChangedDarwinStage(t *testing.T) {
	for _, source := range []string{"running image", "persisted restart"} {
		for _, mismatch := range []string{"device after reboot", "content identity"} {
			for _, route := range []string{"coop", "doctor"} {
				t.Run(source+"/"+mismatch+"/"+route, func(t *testing.T) {
					fixture := newConsecutiveDarwinWakeRestartFixture(t)
					evidence := fixture.currentEvidence
					if mismatch == "device after reboot" {
						evidence.Device++
					} else {
						evidence.SHA256 = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
					}
					if source == "persisted restart" {
						fixture.record.BoundImage = &evidence
						fixture.record.Candidate.SHA256 = evidence.SHA256
						if err := withWakeMutationScopeInDir(fixture.agentDir, func(scope *wakeMutationScope) error {
							return writeWakeRestartRecordAt(scope, fixture.record)
						}); err != nil {
							t.Fatal(err)
						}
					} else if err := os.Remove(filepath.Join(filepath.Dir(fixture.lockPath), wakeRestartFileName)); err != nil {
						t.Fatal(err)
					}
					lock := fixture.stale.Lock
					lock.RunningImageEvidence = &evidence
					writeWakeLockForTest(t, fixture.root, fixture.agent, lock)
					before, err := os.Stat(evidence.ExecutionPath)
					if err != nil {
						t.Fatal(err)
					}
					// Releasing a stale lock must never relax stage deletion authority.
					var changed *wakeRestartStageIdentityError
					if err := cleanupDarwinWakeRestartStage(evidence, true); !errors.As(err, &changed) {
						t.Fatalf("direct stage cleanup = %v, want identity refusal", err)
					}
					if route == "coop" {
						if err := prepareCoopWakeLock(fixture.root, fixture.agent, true, "unused"); err != nil {
							t.Fatalf("coop preflight: %v", err)
						}
					} else {
						stale := inspectWakeLock(fixture.root, fixture.agent)
						result := opsWakeLock{}
						if err := fixStaleWakeLockForDoctor(fixture.root, fixture.agent, &stale, &result); err != nil || !result.Removed {
							t.Fatalf("doctor removed=%v: %v", result.Removed, err)
						}
					}
					if _, err := os.Lstat(fixture.lockPath); !os.IsNotExist(err) {
						t.Fatalf("stale lock survived: %v", err)
					}
					after, err := os.Stat(evidence.ExecutionPath)
					if err != nil || !os.SameFile(before, after) {
						t.Fatalf("unmatched stage was not preserved: %v", err)
					}
					if actual, err := captureWakeImageEvidence(evidence.ExecutionPath, evidence.EmbeddedVersion); err != nil || !sameDarwinStagedWakeImageEvidence(actual, fixture.currentEvidence) {
						t.Fatalf("preserved stage content changed: %v", err)
					}
					cleanup, err := acquireWakeLockWithOptionsInDir(fixture.agentDir, fixture.root, fixture.agent, wakeLockAcquireOptions{wakeMode: wakeInjectModeNone})
					if err != nil {
						t.Fatalf("fresh wake acquisition after stale removal: %v", err)
					}
					cleanup()
				})
			}
		}
	}
}

func TestChangedDarwinStageDoesNotReleaseUnverifiedWake(t *testing.T) {
	fixture := newConsecutiveDarwinWakeRestartFixture(t)
	evidence := fixture.currentEvidence
	evidence.Device++
	lock := fixture.stale.Lock
	lock.RunningImageEvidence = &evidence
	writeWakeLockForTest(t, fixture.root, fixture.agent, lock)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: true}
	})
	inspection := inspectWakeLock(fixture.root, fixture.agent)
	if inspection.Status != wakeLockUnverified {
		t.Fatalf("wake with unreadable running process identity: %s", inspection.Status)
	}
	if err := fixture.agentDir.withFD(func(fd int) error {
		return reclaimWakeRestartStateForLockRemovalAt(fd, fixture.agentDir, inspection)
	}); err == nil {
		t.Fatal("unverified wake accepted changed stage")
	}
	if _, err := os.Stat(fixture.lockPath); err != nil {
		t.Fatalf("unverified lock was removed: %v", err)
	}
}

// The same identity mismatch also trapped doctor in a retry loop after the
// lock disappeared but a persisted restart record remained.
func TestDoctorPreservesChangedDarwinStageWithoutLock(t *testing.T) {
	fixture := newConsecutiveDarwinWakeRestartFixture(t)
	evidence := fixture.currentEvidence
	evidence.Device++
	fixture.record.BoundImage = &evidence
	if err := withWakeMutationScopeInDir(fixture.agentDir, func(scope *wakeMutationScope) error {
		return writeWakeRestartRecordAt(scope, fixture.record)
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(fixture.lockPath); err != nil {
		t.Fatal(err)
	}
	if err := fixWakeRestartResidueWithoutLock(fixture.root, fixture.agent); err != nil {
		t.Fatalf("doctor could not retire abandoned record: %v", err)
	}
	if _, err := os.Stat(evidence.ExecutionPath); err != nil {
		t.Fatalf("changed stage was removed: %v", err)
	}
	entries, err := scanWakeQuarantine(fixture.root)
	if err != nil || len(entries) != 1 {
		t.Fatalf("retained restart evidence: entries=%v err=%v", entries, err)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(fixture.lockPath), wakeRestartFileName)); !os.IsNotExist(err) {
		t.Fatalf("abandoned record still active: %v", err)
	}
	if err := fixWakeRestartResidueWithoutLock(fixture.root, fixture.agent); err != nil {
		t.Fatalf("repeated doctor fix entered a failure loop: %v", err)
	}
	cleanup, err := acquireWakeLockWithOptionsInDir(fixture.agentDir, fixture.root, fixture.agent, wakeLockAcquireOptions{wakeMode: wakeInjectModeNone})
	if err != nil {
		t.Fatalf("fresh wake acquisition: %v", err)
	}
	cleanup()
}
