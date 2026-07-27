//go:build darwin

package cli

import (
	"fmt"
	"time"
)

const wakeStartedTimestampUncertainty = time.Second

func inspectWakeBinaryStalenessPlatform(
	inspection wakeLockInspection,
	current resolvedWakeBinary,
) (wakeBinaryStaleness, error) {
	started, err := time.Parse(time.RFC3339Nano, inspection.Lock.Started)
	if err != nil {
		return wakeBinaryStaleness{}, fmt.Errorf("parse wake Started timestamp: %w", err)
	}
	if current.Info == nil {
		return wakeBinaryStaleness{}, fmt.Errorf("current amq executable metadata unavailable")
	}
	currentIdentity, ok := captureWakeFileIdentity(current.Info)
	if !ok || currentIdentity.Device == 0 || currentIdentity.Inode == 0 {
		return wakeBinaryStaleness{}, fmt.Errorf("current amq executable identity unavailable")
	}

	// Started is currently written with RFC3339 second precision. Require the
	// binary mtime to be strictly beyond that full uncertainty window. This is
	// only a timestamp heuristic: package extraction can preserve or alter mtime.
	return wakeBinaryStaleness{
		Stale:  current.Info.ModTime().After(started.Add(wakeStartedTimestampUncertainty)),
		Method: wakeBinaryComparisonStartedMTime,
		Evidence: wakeBinaryEvidence{
			Available:      true,
			Current:        wakeBinaryFileEvidenceFromIdentity(currentIdentity),
			CurrentModTime: current.Info.ModTime().UnixNano(),
		},
	}, nil
}

func wakeBinaryFileEvidenceFromIdentity(identity wakeFileIdentity) wakeBinaryFileEvidence {
	return wakeBinaryFileEvidence(identity)
}
