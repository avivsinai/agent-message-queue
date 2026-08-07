//go:build darwin || linux

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestWakeSelfUpgradeDiagnosticCleanupAfterGenericRetainedFDLockRemoval(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, false)
	diagnosticPath := writeWakeSelfUpgradeDiagnosticForCleanupTest(t, fixture.root, fixture.me)

	if err := withWakeLifecycleGuardInDir(fixture.agentDir, func(dirfd int) error {
		return removeWakeLockIfUnchangedGuardedAt(dirfd, fixture.agentDir, fixture.created)
	}); err != nil {
		t.Fatal(err)
	}
	fixture.assertLockMissing(t)
	assertPathMissingForTest(t, diagnosticPath)
}

func TestWakeSelfUpgradeDiagnosticCleanupAfterGenericPathLockRemoval(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, false)
	diagnosticPath := writeWakeSelfUpgradeDiagnosticForCleanupTest(t, fixture.root, fixture.me)

	if err := removeWakeLockIfUnchanged(fixture.created); err != nil {
		t.Fatal(err)
	}
	fixture.assertLockMissing(t)
	assertPathMissingForTest(t, diagnosticPath)
}

func TestWakeSelfUpgradeDiagnosticCleanupAfterAuthoritativeRelease(t *testing.T) {
	fixture := newAuthoritativeWakePreparedCleanupFixture(t)
	diagnosticPath := writeWakeSelfUpgradeDiagnosticForCleanupTest(t, fixture.root, fixture.me)

	if err := fixture.release(); err != nil {
		t.Fatal(err)
	}
	fixture.assertReleasedClaimMissing(t)
	assertPathMissingForTest(t, diagnosticPath)
}

func TestWakeSelfUpgradeDiagnosticCleanupWithoutLockDoesNotQuarantine(t *testing.T) {
	root := secureTempDirForTest(t)
	const agent = "codex"
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, agent); err != nil {
		t.Fatal(err)
	}
	diagnosticPath := writeWakeSelfUpgradeDiagnosticForCleanupTest(t, root, agent)

	if err := fixWakeRestartResidueWithoutLock(root, agent); err != nil {
		t.Fatal(err)
	}
	assertPathMissingForTest(t, diagnosticPath)
	quarantined, err := filepath.Glob(diagnosticPath + ".quarantined.*")
	if err != nil {
		t.Fatal(err)
	}
	if len(quarantined) != 0 {
		t.Fatalf("self-upgrade diagnostic was quarantined: %v", quarantined)
	}
}

func TestWakeSelfUpgradeDiagnosticCleanupFailureIsResidueAfterLockCommit(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, false)
	diagnosticPath := writeWakeSelfUpgradeDiagnosticForCleanupTest(t, fixture.root, fixture.me)
	if err := os.Remove(diagnosticPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(diagnosticPath, 0o700); err != nil {
		t.Fatal(err)
	}

	var committed bool
	err := withWakeLifecycleGuardInDir(fixture.agentDir, func(dirfd int) error {
		var removeErr error
		committed, removeErr = removeWakeLockIfUnchangedGuardedAtStatus(dirfd, fixture.agentDir, fixture.created)
		return removeErr
	})
	if !committed {
		t.Fatal("lock removal was not committed")
	}
	fixture.assertLockMissing(t)
	var residue *wakeLockResidueError
	if !errors.As(err, &residue) || residue.residue != wakeLockResidueSelfUpgradeDiagnostic {
		t.Fatalf("cleanup error = %v, want self-upgrade diagnostic residue", err)
	}
}

func TestWakeSelfUpgradeDiagnosticCleanupFailureDoesNotBlockPlainLockRemoval(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, false)
	diagnosticPath := writeWakeSelfUpgradeDiagnosticForCleanupTest(t, fixture.root, fixture.me)
	if err := os.Remove(diagnosticPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(diagnosticPath, 0o700); err != nil {
		t.Fatal(err)
	}

	stderr := captureWakeStderr(t, func() {
		if err := withWakeLifecycleGuardInDir(fixture.agentDir, func(dirfd int) error {
			return removeWakeLockIfUnchangedGuardedAt(dirfd, fixture.agentDir, fixture.created)
		}); err != nil {
			t.Fatalf("plain lock removal blocked on diagnostic residue: %v", err)
		}
	})
	fixture.assertLockMissing(t)
	if !strings.Contains(stderr, "left diagnostic-only self-upgrade residue") {
		t.Fatalf("cleanup warning missing residue: %q", stderr)
	}
}

func TestWakeSelfUpgradeDiagnosticCleanupFailureDoesNotStopGenericCleanup(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, false)
	diagnosticPath := writeWakeSelfUpgradeDiagnosticForCleanupTest(t, fixture.root, fixture.me)
	if err := os.Remove(diagnosticPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(diagnosticPath, 0o700); err != nil {
		t.Fatal(err)
	}

	err := fixture.cleanupNow()
	var residue *wakeLockResidueError
	if !errors.As(err, &residue) || residue.residue != wakeLockResidueSelfUpgradeDiagnostic {
		t.Fatalf("generic cleanup error = %v, want self-upgrade diagnostic residue", err)
	}
	fixture.assertLockMissing(t)
	assertPathMissingForTest(t, fixture.preparedPath)
}

func TestWakeSelfUpgradeDiagnosticCleanupFailureDoesNotBlockAuthoritativeRelease(t *testing.T) {
	fixture := newAuthoritativeWakePreparedCleanupFixture(t)
	diagnosticPath := writeWakeSelfUpgradeDiagnosticForCleanupTest(t, fixture.root, fixture.me)
	if err := os.Remove(diagnosticPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(diagnosticPath, 0o700); err != nil {
		t.Fatal(err)
	}

	stderr := captureWakeStderr(t, func() {
		if err := fixture.release(); err != nil {
			t.Fatalf("authoritative release blocked on diagnostic residue: %v", err)
		}
	})
	fixture.assertReleasedClaimMissing(t)
	assertPathMissingForTest(t, fixture.preparedPath)
	if !strings.Contains(stderr, "left diagnostic-only self-upgrade residue") {
		t.Fatalf("authoritative cleanup warning missing residue: %q", stderr)
	}
}

func TestDoctorReportsDiagnosticResidueAfterRemovingStaleLock(t *testing.T) {
	fixture := newGenericWakePreparedCleanupFixture(t, false)
	diagnosticPath := writeWakeSelfUpgradeDiagnosticForCleanupTest(t, fixture.root, fixture.me)
	if err := os.Remove(diagnosticPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(diagnosticPath, 0o700); err != nil {
		t.Fatal(err)
	}
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo { return wakeProcessInfo{PID: pid} })

	result := runOpsChecks(fixture.root, "test", true)
	if len(result.WakeLocks) != 1 {
		t.Fatalf("doctor wake locks = %#v, want one result", result.WakeLocks)
	}
	lock := result.WakeLocks[0]
	if lock.Status != "error" || !lock.Removed || !strings.Contains(lock.Reason, "self-upgrade diagnostic") {
		t.Fatalf("doctor residue result = %#v, want removed error", lock)
	}
	fixture.assertLockMissing(t)
}

func writeWakeSelfUpgradeDiagnosticForCleanupTest(t *testing.T, root, agent string) string {
	t.Helper()
	path := filepath.Join(fsq.AgentBase(root, agent), wakeSelfUpgradeFileName)
	if err := os.WriteFile(path, []byte("diagnostic-only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
