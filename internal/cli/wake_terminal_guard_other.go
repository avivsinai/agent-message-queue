//go:build !darwin && !linux

package cli

func authorizeTerminalWritePlatform(*wakeConfig) bool {
	return true
}

func isWakeTerminalControlStopped(error) bool {
	return false
}
