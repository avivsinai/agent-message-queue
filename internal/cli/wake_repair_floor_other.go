//go:build !darwin && !linux

package cli

import "fmt"

func validateWakeRepairFloorAvailable(
	root, me string,
	inspection wakeLockInspection,
	target wakeTarget,
) error {
	return fmt.Errorf("wake repair is not supported on this platform")
}
