//go:build darwin

package cli

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func inspectWakeProcessPlatform(pid int) wakeProcessInfo {
	info := wakeProcessInfo{PID: pid}
	if !processAlive(pid) {
		return info
	}
	info.Running = true

	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		info.InspectError = err
		return info
	}
	if kp == nil {
		info.Running = false
		return info
	}

	sec, nsec := kp.Proc.P_starttime.Unix()
	info.StartToken = fmt.Sprintf("%d.%09d", sec, nsec)
	info.Executable = nulTerminatedString(kp.Proc.P_comm[:])

	if boot, err := unix.SysctlTimeval("kern.boottime"); err == nil && boot != nil {
		bsec, bnsec := boot.Unix()
		info.BootID = fmt.Sprintf("%d.%09d", bsec, bnsec)
	}

	return info
}

func nulTerminatedString(raw []byte) string {
	for i, b := range raw {
		if b == 0 {
			return string(raw[:i])
		}
	}
	return string(raw)
}
