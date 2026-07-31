//go:build darwin

package cli

import (
	"bytes"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// SZOMB from <sys/proc.h>: the process has exited and is waiting for its
// parent to reap it. kill(pid, 0) still succeeds for this state.
const darwinProcessStateZombie int8 = 5
const darwinNoControllingTerminal int32 = -1

// Values from Apple's XNU proc_info interface. proc_pidpath in libproc is a
// wrapper around this exact CGO-free syscall.
const (
	darwinSysProcInfo            = 336
	darwinProcInfoCallPIDInfo    = 0x2
	darwinProcPIDPathInfo        = 11
	darwinProcPIDPathInfoMaxSize = 4 * unix.PathMax
)

var (
	readDarwinKinfoProc             = unix.SysctlKinfoProc
	readDarwinProcessExecutablePath = readDarwinProcessExecutablePathDefault
	readDarwinBootSessionUUID       = func() (string, error) {
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
	if path, pathErr := readDarwinProcessExecutablePath(pid); pathErr == nil {
		info.ExecutablePath = path
	}
	info.ControllingTerminalKnown = true
	info.HasControllingTerminal = kp.Eproc.Tdev != darwinNoControllingTerminal
	info.ControllingTerminalDevice = kp.Eproc.Tdev

	info.BootID, info.LegacyBootID = darwinBootIdentity()

	return info
}

func readDarwinProcessExecutablePathDefault(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("invalid process id %d", pid)
	}
	buffer := make([]byte, darwinProcPIDPathInfoMaxSize)
	_, _, errno := unix.Syscall6(
		darwinSysProcInfo,
		darwinProcInfoCallPIDInfo,
		uintptr(pid),
		darwinProcPIDPathInfo,
		0,
		uintptr(unsafe.Pointer(&buffer[0])),
		uintptr(len(buffer)),
	)
	runtime.KeepAlive(buffer)
	if errno != 0 {
		return "", fmt.Errorf("proc_pidpath pid %d: %w", pid, errno)
	}
	end := bytes.IndexByte(buffer, 0)
	if end <= 0 {
		return "", fmt.Errorf("proc_pidpath pid %d returned an unterminated path", pid)
	}
	path := string(buffer[:end])
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", fmt.Errorf("proc_pidpath pid %d returned a non-canonical absolute path", pid)
	}
	return path, nil
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
