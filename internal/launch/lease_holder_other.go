//go:build !darwin && !linux

package launch

import "fmt"

func inspectProcessPlatform(pid int) processInfo {
	info := processInfo{PID: pid, Running: processAlive(pid)}
	if info.Running {
		info.InspectError = fmt.Errorf("process start time unavailable on this platform")
	}
	return info
}
