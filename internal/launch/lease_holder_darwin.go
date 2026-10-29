//go:build darwin

package launch

import (
	"fmt"
	"strconv"
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

func parentProcessID(pid int) (int, error) {
	kp, err := readDarwinKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, err
	}
	if kp == nil || int(kp.Proc.P_pid) != pid {
		return 0, fmt.Errorf("process %d is not running", pid)
	}
	return int(kp.Eproc.Ppid), nil
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

// pidNamespace is empty: this platform has no pid namespaces.
func pidNamespace() string { return "" }

// processStartUnix is the wall-clock start second of a start token, which
// on Darwin is the kernel's p_starttime as "sec.nsec".
func processStartUnix(token string) (int64, error) {
	sec, _, ok := strings.Cut(token, ".")
	value, err := strconv.ParseInt(sec, 10, 64)
	if !ok || err != nil || value <= 0 {
		return 0, fmt.Errorf("process start token %q is malformed", token)
	}
	return value, nil
}

// startedNoLaterThan reports whether start token a is at or before b. A
// Darwin token is the kernel's p_starttime as "sec.nsec"; both parts are
// compared as integers.
func startedNoLaterThan(a, b string) (bool, error) {
	aSec, aNsec, err := darwinStartTime(a)
	if err != nil {
		return false, err
	}
	bSec, bNsec, err := darwinStartTime(b)
	if err != nil {
		return false, err
	}
	return aSec < bSec || (aSec == bSec && aNsec <= bNsec), nil
}

func darwinStartTime(token string) (sec, nsec int64, err error) {
	secText, nsecText, ok := strings.Cut(token, ".")
	if ok {
		sec, err = strconv.ParseInt(secText, 10, 64)
	}
	if ok && err == nil {
		nsec, err = strconv.ParseInt(nsecText, 10, 64)
	}
	if !ok || err != nil || sec < 0 || nsec < 0 || nsec >= 1_000_000_000 {
		return 0, 0, fmt.Errorf("process start token %q is malformed", token)
	}
	return sec, nsec, nil
}
