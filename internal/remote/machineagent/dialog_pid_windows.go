//go:build windows

package machineagent

// pidAlive reports true: without a cheap liveness probe here, a dialog is
// closed only by its seam.
func pidAlive(int) bool { return true }
