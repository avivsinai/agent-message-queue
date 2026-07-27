//go:build linux

package cli

func sameWakeTerminalAsCurrent(inspection wakeLockInspection) bool {
	return sameWakeTTYPathAsCurrent(inspection)
}
