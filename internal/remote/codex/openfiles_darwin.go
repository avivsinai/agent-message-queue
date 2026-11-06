//go:build darwin

package codex

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// proc_info(2) as libproc's proc_pidinfo and proc_pidfdinfo call it.
const (
	darwinSysProcInfo         = 336
	darwinProcInfoCallPIDInfo = 2
	darwinProcInfoCallFDInfo  = 3
	darwinProcPIDListFDs      = 1
	darwinProcPIDFDVnodePath  = 2
	darwinProxFDTypeVnode     = 1
	// struct proc_fdinfo is {int32 fd; uint32 type}. struct
	// vnode_fdinfowithpath is a 24-byte proc_fileinfo, a 152-byte
	// vnode_info and a MAXPATHLEN (1024) path.
	darwinFDInfoSize      = 8
	darwinVnodePathOffset = 24 + 152
	darwinVnodeFDInfoSize = darwinVnodePathOffset + 1024
	darwinOpenFilesMaxFDs = 4096
)

// openFilePaths is the path of each vnode process pid holds open.
func openFilePaths(pid int) ([]string, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("invalid process id %d", pid)
	}
	list := make([]byte, darwinOpenFilesMaxFDs*darwinFDInfoSize)
	n, _, errno := unix.Syscall6(darwinSysProcInfo, darwinProcInfoCallPIDInfo, uintptr(pid),
		darwinProcPIDListFDs, 0, uintptr(unsafe.Pointer(&list[0])), uintptr(len(list)))
	runtime.KeepAlive(list)
	if errno != 0 {
		return nil, fmt.Errorf("proc_pidinfo pid %d: %w", pid, errno)
	}
	if n >= uintptr(len(list)) {
		return nil, fmt.Errorf("process %d holds more than %d descriptors", pid, darwinOpenFilesMaxFDs)
	}
	var paths []string
	var info [darwinVnodeFDInfoSize]byte
	for i := 0; i+darwinFDInfoSize <= int(n); i += darwinFDInfoSize {
		fd := int32(binary.LittleEndian.Uint32(list[i:]))
		if binary.LittleEndian.Uint32(list[i+4:]) != darwinProxFDTypeVnode {
			continue
		}
		got, _, errno := unix.Syscall6(darwinSysProcInfo, darwinProcInfoCallFDInfo, uintptr(pid),
			darwinProcPIDFDVnodePath, uintptr(fd), uintptr(unsafe.Pointer(&info[0])), uintptr(len(info)))
		runtime.KeepAlive(&info)
		if errno != 0 || got != uintptr(len(info)) {
			continue
		}
		path := info[darwinVnodePathOffset:]
		if end := bytes.IndexByte(path, 0); end >= 0 {
			path = path[:end]
		}
		if len(path) > 0 {
			paths = append(paths, string(path))
		}
	}
	return paths, nil
}
