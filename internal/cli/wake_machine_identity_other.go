//go:build !darwin && !linux

package cli

func readWakeMachineIDPlatform() string {
	return ""
}

func readWakeBootIDPlatform() string {
	return ""
}
