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

	// Started is currently written with RFC3339 second precision. Require the
	// binary mtime to be strictly beyond that full uncertainty window. This is
	// only a timestamp heuristic: package extraction can preserve or alter mtime.
	return wakeBinaryStaleness{
		Stale:  current.Info.ModTime().After(started.Add(wakeStartedTimestampUncertainty)),
		Method: wakeBinaryComparisonStartedMTime,
	}, nil
}
