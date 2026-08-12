//go:build !darwin && !linux

package cli

func readWakeMachineIDPlatform() string {
	return ""
}
