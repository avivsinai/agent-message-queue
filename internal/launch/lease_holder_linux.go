//go:build linux

package launch

import (
	"fmt"
	"os"
	"strconv"
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

func parentProcessID(pid int) (int, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	stat := string(data)
	endComm := strings.LastIndex(stat, ")")
	if endComm < 0 || endComm+2 >= len(stat) {
		return 0, fmt.Errorf("malformed proc stat")
	}
	fields := strings.Fields(stat[endComm+2:])
	if len(fields) < 2 {
		return 0, fmt.Errorf("proc stat missing ppid")
	}
	return strconv.Atoi(fields[1])
}

// pidNamespace is the inode of this process's pid namespace, or empty when
// it cannot be read.
func pidNamespace() string {
	link, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return ""
	}
	inode, ok := strings.CutPrefix(link, "pid:[")
	if !ok {
		return ""
	}
	return strings.TrimSuffix(inode, "]")
}

// linuxUserHZ is the clock-tick rate of /proc/<pid>/stat starttime, the
// fixed USER_HZ of the Linux userspace ABI.
const linuxUserHZ = 100

// processStartUnix is the wall-clock start second of a start token, which
// on Linux is stat starttime in clock ticks since boot, added to the boot
// time from /proc/stat (the same computation as ps lstart).
func processStartUnix(token string) (int64, error) {
	ticks, err := strconv.ParseInt(token, 10, 64)
	if err != nil || ticks < 0 {
		return 0, fmt.Errorf("process start token %q is malformed", token)
	}
	stat, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(stat), "\n") {
		if value, ok := strings.CutPrefix(line, "btime "); ok {
			boot, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil || boot <= 0 {
				break
			}
			return boot + ticks/linuxUserHZ, nil
		}
	}
	return 0, fmt.Errorf("boot time unavailable")
}

// startedNoLaterThan reports whether start token a is at or before b. A
// Linux token is stat starttime in clock ticks since boot, so a parent and
// a child forked within one tick have equal tokens.
func startedNoLaterThan(a, b string) (bool, error) {
	aTicks, err := strconv.ParseInt(a, 10, 64)
	if err != nil || aTicks < 0 {
		return false, fmt.Errorf("process start token %q is malformed", a)
	}
	bTicks, err := strconv.ParseInt(b, 10, 64)
	if err != nil || bTicks < 0 {
		return false, fmt.Errorf("process start token %q is malformed", b)
	}
	return aTicks <= bTicks, nil
}
