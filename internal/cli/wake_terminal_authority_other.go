//go:build !darwin && !linux

package cli

func isWakeTerminalForegroundPGRPChanged(error) bool {
	return false
}

func isWakeTerminalAuthorityLoss(error) bool {
	return false
}
