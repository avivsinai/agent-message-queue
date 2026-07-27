//go:build linux

package cli

import (
	"fmt"
	"os"
)

func inspectWakeBinaryStalenessPlatform(
	inspection wakeLockInspection,
	current resolvedWakeBinary,
) (wakeBinaryStaleness, error) {
	if current.Info == nil {
		return wakeBinaryStaleness{}, fmt.Errorf("current amq executable metadata unavailable")
	}
	runningInfo, err := os.Stat(fmt.Sprintf("/proc/%d/exe", inspection.PID))
	if err != nil {
		return wakeBinaryStaleness{}, fmt.Errorf("stat wake process executable: %w", err)
	}
	runningIdentity, ok := captureWakeFileIdentity(runningInfo)
	if !ok || runningIdentity.Device == 0 || runningIdentity.Inode == 0 {
		return wakeBinaryStaleness{}, fmt.Errorf("wake process executable identity unavailable")
	}
	currentIdentity, ok := captureWakeFileIdentity(current.Info)
	if !ok || currentIdentity.Device == 0 || currentIdentity.Inode == 0 {
		return wakeBinaryStaleness{}, fmt.Errorf("current amq executable identity unavailable")
	}
	return wakeBinaryStaleness{
		Stale: runningIdentity.Device != currentIdentity.Device ||
			runningIdentity.Inode != currentIdentity.Inode,
		Method: wakeBinaryComparisonExactIdentity,
		Evidence: wakeBinaryEvidence{
			Available: true,
			Running:   wakeBinaryFileEvidenceFromIdentity(runningIdentity),
			Current:   wakeBinaryFileEvidenceFromIdentity(currentIdentity),
		},
	}, nil
}

func wakeBinaryFileEvidenceFromIdentity(identity wakeFileIdentity) wakeBinaryFileEvidence {
	return wakeBinaryFileEvidence(identity)
}
