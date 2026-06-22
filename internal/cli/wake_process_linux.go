//go:build linux

package cli

import (
	"fmt"
	"os"
	"strings"
)

func inspectWakeProcessPlatform(pid int) wakeProcessInfo {
	info := wakeProcessInfo{PID: pid}
	if !processAlive(pid) {
		return info
	}
	info.Running = true

	if bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		info.BootID = strings.TrimSpace(string(bootID))
	}

	statPath := fmt.Sprintf("/proc/%d/stat", pid)
	if data, err := os.ReadFile(statPath); err == nil {
		if token, parseErr := linuxProcStartToken(string(data)); parseErr == nil {
			info.StartToken = token
		} else {
			info.InspectError = parseErr
		}
	} else {
		info.InspectError = err
	}

	if exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil {
		info.Executable = exe
	} else if info.InspectError == nil {
		info.InspectError = err
	}

	if cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		info.Args = splitProcCmdline(cmdline)
	}

	return info
}

func linuxProcStartToken(stat string) (string, error) {
	endComm := strings.LastIndex(stat, ")")
	if endComm < 0 || endComm+2 >= len(stat) {
		return "", fmt.Errorf("malformed proc stat")
	}
	fields := strings.Fields(stat[endComm+2:])
	// fields[0] is stat field 3; starttime is field 22.
	const startTimeIndex = 22 - 3
	if len(fields) <= startTimeIndex {
		return "", fmt.Errorf("proc stat missing starttime")
	}
	return fields[startTimeIndex], nil
}

func splitProcCmdline(raw []byte) []string {
	parts := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	args := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			args = append(args, part)
		}
	}
	return args
}
