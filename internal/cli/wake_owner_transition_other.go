//go:build !darwin && !linux

package cli

func classifyWakeClaimForGenericTransition(inspection wakeLockInspection) wakeClaimClass {
	if !inspection.Exists {
		return wakeClaimAbsent
	}
	if inspection.fileInfo == nil {
		return wakeClaimInvalid
	}
	if err := validateWakeLockInspectionStateBindingJSON(inspection); err != nil {
		return wakeClaimInvalid
	}
	if wakeLockHasOwnerMarkers(inspection) {
		return wakeClaimAuthoritative
	}
	return wakeClaimGeneric
}
