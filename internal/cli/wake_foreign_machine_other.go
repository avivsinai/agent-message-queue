//go:build !darwin && !linux

package cli

// Wake is unsupported on this platform, so no lock is classified foreign.
func wakeForeignGenericLock(wakeLockInspection) bool { return false }
