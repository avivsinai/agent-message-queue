//go:build !darwin

package cli

func isLiveRawOrphan(wakeLockInspection) bool { return false }
