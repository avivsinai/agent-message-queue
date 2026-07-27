//go:build !darwin && !linux

package cli

func isLiveRawOrphan(wakeLockInspection) bool { return false }
