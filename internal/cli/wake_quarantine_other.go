//go:build !darwin && !linux

package cli

import (
	"fmt"
	"time"
)

type wakeQuarantineCleanupCandidate struct {
	Path string
}

func checkWakeQuarantine(string, time.Time) (opsWakeQuarantine, error) {
	return opsWakeQuarantine{}, nil
}

func findWakeQuarantineOlderThan(string, time.Time) ([]wakeQuarantineCleanupCandidate, error) {
	return nil, fmt.Errorf("wake quarantine cleanup is unsupported on this platform")
}

func removeWakeQuarantineCandidate(string, wakeQuarantineCleanupCandidate) error {
	return fmt.Errorf("wake quarantine cleanup is unsupported on this platform")
}
