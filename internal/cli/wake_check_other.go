//go:build !darwin && !linux

package cli

func decorateOpsWakeLockWithWakeCheck(
	string,
	*opsWakeLock,
	wakeLockInspection,
	bool,
) {
}
