//go:build darwin

package launch

import (
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

const darwinProcessStateZombie int8 = 5

var (
	readDarwinKinfoProc       = unix.SysctlKinfoProc
	readDarwinBootSessionUUID = func() (string, error) { return unix.Sysctl("kern.bootsessionuuid") }
	readDarwinBootTime        = func() (*unix.Timeval, error) { return unix.SysctlTimeval("kern.boottime") }
)

func inspectProcessPlatform(pid int) processInfo {
	info := processInfo{PID: pid}
	if !processAlive(pid) {
		return info
	}
	info.Running = true
	kp, err := readDarwinKinfoProc("kern.proc.pid", pid)
	if err != nil {
		info.InspectError = err
		return info
	}
	if kp == nil || kp.Proc.P_stat == darwinProcessStateZombie {
		info.Running = false
		return info
	}
	sec, nsec := kp.Proc.P_starttime.Unix()
	info.StartToken = fmt.Sprintf("%d.%09d", sec, nsec)
	info.BootID = darwinBootID()
	return info
}

func darwinBootID() string {
	if sessionUUID, err := readDarwinBootSessionUUID(); err == nil {
		sessionUUID = strings.TrimSpace(sessionUUID)
		if sessionUUID != "" && !strings.ContainsRune(sessionUUID, 0) {
			return sessionUUID
		}
	}
	if boot, err := readDarwinBootTime(); err == nil && boot != nil {
		sec, nsec := boot.Unix()
		return fmt.Sprintf("%d.%09d", sec, nsec)
	}
	return ""
}
