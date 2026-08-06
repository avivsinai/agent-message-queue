//go:build !darwin && !linux

package cli

func doctorWakeLockOnCurrentTerminal(wakeLockInspection) bool {
	return false
}
