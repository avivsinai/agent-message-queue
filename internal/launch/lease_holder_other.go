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

func parentProcessID(int) (int, error) {
	return 0, fmt.Errorf("process ancestry unavailable on this platform")
}

// pidNamespace is empty: this platform has no pid namespaces.
func pidNamespace() string { return "" }

func processStartUnix(string) (int64, error) {
	return 0, fmt.Errorf("process start time unavailable on this platform")
}

// startedNoLaterThan fails closed: this platform has no start token to order.
func startedNoLaterThan(string, string) (bool, error) {
	return false, fmt.Errorf("process start time unavailable on this platform")
}
