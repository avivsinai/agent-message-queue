//go:build !darwin && !linux

package cli

import "os"

func isWakeTerminalForegroundPGRPChanged(error) bool {
	return false
}

func isWakeTerminalAuthorityLoss(error) bool {
	return false
}

// wakeFileIsForeground keeps plain wake output where amq wake cannot run.
func wakeFileIsForeground(*os.File) bool {
	return true
}
