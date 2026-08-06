//go:build darwin || linux

package cli

func isLiveRawOrphan(inspection wakeLockInspection) bool {
	return inspection.Process.Running &&
		inspection.IdentityConfirmed &&
		inspection.Lock.WakeMode != wakeTargetInjectVia &&
		!wakeLockHasOwnerMarkers(inspection) &&
		!wakeLockHasUsableNotificationPath(inspection)
}
