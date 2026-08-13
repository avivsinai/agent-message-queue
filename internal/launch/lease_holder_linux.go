//go:build linux

package launch

import (
	"fmt"
	"os"
	"strings"
)

func inspectProcessPlatform(pid int) processInfo {
	info := processInfo{PID: pid}
	if !processAlive(pid) {
		return info
	}
	info.Running = true
	if bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		info.BootID = strings.TrimSpace(string(bootID))
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		info.InspectError = err
		return info
	}
	token, state, err := linuxProcIdentity(string(data))
	if err != nil {
		info.InspectError = err
		return info
	}
	if state == "Z" {
		info.Running = false
		return info
	}
	info.StartToken = token
	return info
}

func linuxProcIdentity(stat string) (startToken string, state string, err error) {
	endComm := strings.LastIndex(stat, ")")
	if endComm < 0 || endComm+2 >= len(stat) {
		return "", "", fmt.Errorf("malformed proc stat")
	}
	fields := strings.Fields(stat[endComm+2:])
	const startTimeIndex = 22 - 3
	if len(fields) <= startTimeIndex {
		return "", "", fmt.Errorf("proc stat missing starttime")
	}
	if len(fields[0]) != 1 {
		return "", "", fmt.Errorf("proc stat has malformed state")
	}
	return fields[startTimeIndex], fields[0], nil
}
