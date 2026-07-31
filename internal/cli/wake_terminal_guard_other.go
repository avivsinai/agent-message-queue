//go:build !darwin && !linux

package cli

func authorizeTerminalWritePlatform(*wakeConfig) bool {
	return true
}

func authorizeTerminalWritePlatformState(*wakeConfig) (bool, error) {
	return true, nil
}

func isWakeTerminalControlStopped(error) bool {
	return false
}
