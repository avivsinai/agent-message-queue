//go:build !darwin && !linux

package launch

import (
	"fmt"
	"time"
)

func inspectProcessPlatform(pid int) processInfo {
	info := processInfo{PID: pid, Running: processAlive(pid)}
	if info.Running {
		info.InspectError = fmt.Errorf("process start time unavailable on this platform")
	}
	return info
}

func parentProcessIDPlatform(int) (int, error) {
	return 0, fmt.Errorf("process ancestry unavailable on this platform")
}

// pidNamespace is empty: this platform has no pid namespaces.
func pidNamespace() string { return "" }

func processStartTime(string) (time.Time, error) {
	return time.Time{}, fmt.Errorf("process start time unavailable on this platform")
}
