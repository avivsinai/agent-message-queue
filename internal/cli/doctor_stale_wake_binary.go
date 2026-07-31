package cli

import (
	"fmt"
	"os"
	"slices"

	"github.com/avivsinai/agent-message-queue/internal/update"
)

type wakeBinaryComparisonMethod string

const (
	wakeBinaryComparisonExactIdentity      wakeBinaryComparisonMethod = "device_inode"
	wakeBinaryComparisonDarwinProcessImage wakeBinaryComparisonMethod = "darwin_process_image"
	wakeBinaryComparisonStartedMTime       wakeBinaryComparisonMethod = "started_mtime_heuristic"
)

type wakeBinaryStaleness struct {
	Stale    bool
	Method   wakeBinaryComparisonMethod
	Evidence wakeBinaryEvidence
}

type wakeBinaryEvidence struct {
	Available      bool
	Running        wakeBinaryFileEvidence
	Current        wakeBinaryFileEvidence
	CurrentModTime int64
}

type wakeBinaryFileEvidence struct {
	Device    uint64
	Inode     uint64
	CTimeSec  int64
	CTimeNsec int64
}

type resolvedWakeBinary struct {
	Info os.FileInfo
}

var (
	inspectWakeBinaryStaleness = inspectWakeBinaryStalenessDefault
	resolveWakeExecutablePath  = update.ExecutablePath
)

func inspectWakeBinaryStalenessDefault(inspection wakeLockInspection) (wakeBinaryStaleness, error) {
	current, err := resolveWakeBinary()
	if err != nil {
		return wakeBinaryStaleness{}, err
	}
	return inspectWakeBinaryStalenessPlatform(inspection, current)
}

func resolveWakeBinary() (resolvedWakeBinary, error) {
	path, resolved, err := resolveWakeExecutablePath()
	if err != nil {
		return resolvedWakeBinary{}, fmt.Errorf("resolve current amq executable: %w", err)
	}
	if resolved != "" {
		path = resolved
	}
	info, err := os.Stat(path)
	if err != nil {
		return resolvedWakeBinary{}, fmt.Errorf("stat current amq executable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return resolvedWakeBinary{}, fmt.Errorf("current amq executable is not a regular file")
	}
	return resolvedWakeBinary{Info: info}, nil
}

func checkStaleWakeBinaryHint(inspection wakeLockInspection) (opsHint, bool) {
	if !inspection.Exists ||
		inspection.Status != wakeLockValid ||
		!inspection.IdentityConfirmed ||
		!inspection.Process.Running {
		return opsHint{}, false
	}

	comparison, err := inspectWakeBinaryStaleness(inspection)
	if err != nil || !comparison.Stale {
		return opsHint{}, false
	}
	if comparison.Method != wakeBinaryComparisonExactIdentity &&
		comparison.Method != wakeBinaryComparisonDarwinProcessImage &&
		comparison.Method != wakeBinaryComparisonStartedMTime {
		return opsHint{}, false
	}

	// A PID can exit and be reused while its executable is inspected. Re-read
	// both the lock generation and kernel process identity before reporting.
	confirmed := inspectWakeLock(inspection.Root, inspection.Agent)
	if !sameWakeBinaryInspection(inspection, confirmed) ||
		!confirmed.IdentityConfirmed ||
		!sameWakeBinaryProcessEvidence(inspection.Process, confirmed.Process) {
		return opsHint{}, false
	}
	recheckedComparison, err := inspectWakeBinaryStaleness(confirmed)
	if err != nil ||
		!recheckedComparison.Stale ||
		recheckedComparison.Method != comparison.Method ||
		!sameWakeBinaryEvidence(comparison.Evidence, recheckedComparison.Evidence) {
		return opsHint{}, false
	}
	final := inspectWakeLock(confirmed.Root, confirmed.Agent)
	if !sameWakeBinaryInspection(confirmed, final) ||
		!final.IdentityConfirmed ||
		!sameWakeBinaryProcessEvidence(confirmed.Process, final.Process) {
		return opsHint{}, false
	}

	remedy := "restart this wake through its owning shell, launchd, systemd, or coop supervisor"
	message := fmt.Sprintf(
		"Wake for agent %q (pid %d) is running a different amq executable; %s.",
		inspection.Agent,
		inspection.PID,
		remedy,
	)
	if comparison.Method == wakeBinaryComparisonStartedMTime {
		message = fmt.Sprintf(
			"Wake for agent %q (pid %d) may be running an older amq binary: "+
				"the current resolved binary is newer than the wake Started timestamp "+
				"(Darwin timestamp heuristic); %s.",
			inspection.Agent,
			inspection.PID,
			remedy,
		)
	}

	return opsHint{
		Code:    "stale_wake_binary",
		Status:  "warn",
		Message: message,
		WakeBinary: &opsWakeBinaryHint{
			Agent:  inspection.Agent,
			PID:    inspection.PID,
			Remedy: remedy,
		},
	}, true
}

func sameWakeBinaryEvidence(first, second wakeBinaryEvidence) bool {
	return first.Available && second.Available && first == second
}

func sameWakeBinaryProcessEvidence(first, second wakeProcessInfo) bool {
	return sameWakeOwnerProcessSnapshot(first, second) &&
		first.Executable == second.Executable &&
		first.ExecutablePath == second.ExecutablePath &&
		slices.Equal(first.Args, second.Args)
}

func sameWakeBinaryInspection(first, second wakeLockInspection) bool {
	if !second.Exists || second.Status != wakeLockValid {
		return false
	}
	if first.PID != second.PID || first.Root != second.Root || first.Agent != second.Agent {
		return false
	}
	return sameWakeLockGeneration(first, second)
}
