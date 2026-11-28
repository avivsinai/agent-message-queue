//go:build darwin

package cli

import (
	"fmt"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Values from Apple's XNU proc_info interface, matching the constants the
// wake process and region readers already use. PROC_PIDCWDINFO (3) returns
// struct proc_vnodepathinfo's vip_cdir field: the cwd vnode and its path.
const (
	darwinProcPIDCWDInfo     = 3
	darwinProcCWDInfoMaxSize = 4 * unix.PathMax
)

// darwinProcessCwd reads a process's working directory with the same
// CGO-free proc_info syscall selfupgrade's proc_pidpath uses.
func darwinProcessCwd(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("invalid process id %d", pid)
	}
	buffer := make([]byte, darwinProcCWDInfoMaxSize)
	read, _, errno := unix.Syscall6(
		darwinSysProcInfo,
		darwinProcInfoCallPIDInfo,
		uintptr(pid),
		darwinProcPIDCWDInfo,
		0,
		uintptr(unsafe.Pointer(&buffer[0])),
		uintptr(len(buffer)),
	)
	runtime.KeepAlive(buffer)
	if errno != 0 {
		return "", fmt.Errorf("proc_pidinfo cwd pid %d: %w", pid, errno)
	}
	if read == 0 {
		return "", nil
	}
	end := 0
	for end < len(buffer) && buffer[end] != 0 {
		end++
	}
	path := string(buffer[:end])
	if path == "" {
		return "", nil
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("proc_pidinfo cwd pid %d returned a relative path", pid)
	}
	return filepath.Clean(path), nil
}
