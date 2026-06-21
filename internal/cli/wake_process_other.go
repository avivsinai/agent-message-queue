//go:build !darwin && !linux

package cli

import "fmt"

func inspectWakeProcessPlatform(pid int) wakeProcessInfo {
	return wakeProcessInfo{
		PID:          pid,
		Running:      true,
		InspectError: fmt.Errorf("wake process inspection is not supported on this platform"),
	}
}
