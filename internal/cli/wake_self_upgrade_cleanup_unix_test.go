//go:build darwin || linux

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

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
