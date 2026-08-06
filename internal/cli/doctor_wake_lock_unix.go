//go:build darwin || linux

package cli

func doctorWakeLockOnCurrentTerminal(inspection wakeLockInspection) bool {
	return sameWakeTTYPathAsCurrent(inspection)
}
