//go:build darwin

package cli

import (
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

// SZOMB from <sys/proc.h>: the process has exited and is waiting for its
// parent to reap it. kill(pid, 0) still succeeds for this state.
const darwinProcessStateZombie int8 = 5

var (
	readDarwinKinfoProc       = unix.SysctlKinfoProc
	readDarwinBootSessionUUID = func() (string, error) {
		return unix.Sysctl("kern.bootsessionuuid")
	}
	readDarwinBootTime = func() (*unix.Timeval, error) {
		return unix.SysctlTimeval("kern.boottime")
	}
)

func inspectWakeProcessPlatform(pid int) wakeProcessInfo {
	info := wakeProcessInfo{PID: pid}
	if !processAlive(pid) {
		return info
	}
	info.Running = true

	kp, err := readDarwinKinfoProc("kern.proc.pid", pid)
	if err != nil {
		info.InspectError = err
		return info
	}
	if kp == nil {
		info.Running = false
		return info
	}
	if kp.Proc.P_stat == darwinProcessStateZombie {
		info.Running = false
		return info
	}

	sec, nsec := kp.Proc.P_starttime.Unix()
	info.StartToken = fmt.Sprintf("%d.%09d", sec, nsec)
	info.Executable = nulTerminatedString(kp.Proc.P_comm[:])

	info.BootID, info.LegacyBootID = darwinBootIdentity()

	return info
}

func darwinBootIdentity() (bootID, legacyBootID string) {
	if boot, err := readDarwinBootTime(); err == nil && boot != nil {
		sec, nsec := boot.Unix()
		legacyBootID = fmt.Sprintf("%d.%09d", sec, nsec)
	}

	if sessionUUID, err := readDarwinBootSessionUUID(); err == nil {
		sessionUUID = strings.TrimSpace(sessionUUID)
		if sessionUUID != "" && !strings.ContainsRune(sessionUUID, 0) {
			return sessionUUID, legacyBootID
		}
	}

	// kern.boottime was AMQ's Darwin boot identity before v0.42. Keep it as a
	// best-effort fallback for macOS versions where bootsessionuuid is absent.
	return legacyBootID, ""
}

func nulTerminatedString(raw []byte) string {
	for i, b := range raw {
		if b == 0 {
			return string(raw[:i])
		}
	}
	return string(raw)
}
