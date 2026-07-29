//go:build !darwin && !linux

package cli

func isWakeTerminalForegroundPGRPChanged(error) bool {
	return false
}
