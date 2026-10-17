//go:build darwin

package launch

import (
	"bytes"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	darwinSysProcInfo          = 336
	darwinProcInfoCallPIDInfo  = 0x2
	darwinProcPIDVnodePathInfo = 9
	// struct vnode_info is 152 bytes and MAXPATHLEN is 1024 on Darwin arm64
	// and amd64; struct proc_vnodepathinfo is the current directory's
	// vnode_info_path followed by the root directory's.
	darwinVnodeInfoSize = 152
	darwinMaxPathLen    = 1024
)

// ProcessCwd is the current working directory of process pid.
func ProcessCwd(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("invalid process id %d", pid)
	}
	var info [2 * (darwinVnodeInfoSize + darwinMaxPathLen)]byte
	read, _, errno := unix.Syscall6(darwinSysProcInfo, darwinProcInfoCallPIDInfo, uintptr(pid),
		darwinProcPIDVnodePathInfo, 0, uintptr(unsafe.Pointer(&info[0])), uintptr(len(info)))
	runtime.KeepAlive(&info)
	if errno != 0 {
		return "", fmt.Errorf("proc_pidinfo pid %d: %w", pid, errno)
	}
	if read != uintptr(len(info)) {
		return "", fmt.Errorf("proc_pidinfo pid %d returned %d bytes, want %d", pid, read, len(info))
	}
	path := info[darwinVnodeInfoSize : darwinVnodeInfoSize+darwinMaxPathLen]
	if end := bytes.IndexByte(path, 0); end >= 0 {
		path = path[:end]
	}
	if len(path) == 0 {
		return "", fmt.Errorf("process %d reports no working directory", pid)
	}
	return string(path), nil
}
