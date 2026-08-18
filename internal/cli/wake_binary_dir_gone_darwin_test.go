//go:build darwin

package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestDoctorFixReclaimsStaleLockAfterDarwinStageParentDisappears(t *testing.T) {
	record := newDarwinWakeRestartStageRecordForTest(t)
	bound, err := bindWakeRestartCandidateAtPlatform(record.Candidate, record.StagePath)
	if err != nil {
		t.Fatal(err)
	}
	evidence := bound.evidence
	if err := bound.file.Close(); err != nil {
		t.Fatal(err)
	}
	bound.file = nil

	root := secureTempDirForTest(t)
	const agent = "codex"
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, agent); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteConfig(
		filepath.Join(root, "meta", "config.json"),
		config.Config{Version: 1, Agents: []string{agent}},
		true,
	); err != nil {
		t.Fatal(err)
	}

	lockPath := writeWakeLockForTest(t, root, agent, wakeLock{
		PID:                  66121,
		Executable:           "/opt/homebrew/bin/amq",
		ImagePath:            evidence.ExecutionPath,
		ImageVersion:         evidence.EmbeddedVersion,
		Generation:           "removed-cellar-stage",
		WakeMode:             wakeInjectModeRaw,
		RunningImageEvidence: &evidence,
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})
	stubWakeCheckRuntime(t, true, "0.64.1")

	stageParent := filepath.Dir(filepath.Dir(record.StagePath))
	if err := os.RemoveAll(stageParent); err != nil {
		t.Fatal(err)
	}

	result := runOpsChecksWithSchema(root, "test", false, wakeCheckSchemaV2)
	if len(result.WakeLocks) != 1 {
		t.Fatalf("doctor wake locks = %#v", result.WakeLocks)
	}
	got := result.WakeLocks[0]
	wantFix := doctorRootCommandForOS(root, "", runtime.GOOS, "--ops", "--fix-wake-locks")
	if got.Status != string(wakeLockStale) ||
		got.Reason != wakeReasonBinaryDirGone ||
		got.RestartStageStatus != wakeRestartStageBinaryDirGone ||
		!strings.Contains(got.RestartStageReason, wakeBinaryDirGoneMessage) ||
		got.Fix != wantFix {
		t.Fatalf("doctor --ops vanished Darwin stage = %#v", got)
	}
	assertNoRawENOENT(t, got.Reason, got.InspectionError, got.RestartStageReason, got.NextAction)

	fixed := runOpsChecks(root, "test", true)
	if len(fixed.WakeLocks) != 1 || fixed.WakeLocks[0].Status != "fixed" || !fixed.WakeLocks[0].Removed {
		t.Fatalf("doctor --fix-wake-locks vanished Darwin stage = %#v", fixed.WakeLocks)
	}
	if _, err := os.Lstat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("stale lock survived Darwin stage-parent fix: %v", err)
	}
}
