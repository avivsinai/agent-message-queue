//go:build darwin

package selfupgrade

import (
	"bytes"
	"fmt"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Values from Apple's XNU proc_info interface. proc_pidpath in libproc is a
// wrapper around this exact CGO-free syscall.
const (
	darwinSysProcInfo            = 336
	darwinProcInfoCallPIDInfo    = 0x2
	darwinProcPIDPathInfo        = 11
	darwinProcPIDPathInfoMaxSize = 4 * unix.PathMax
)

// ProcessExecutablePath is proc_pidpath: the name the kernel holds for the
// image a process runs. It fails with ENOENT once that image has no name.
func ProcessExecutablePath(pid int) (string, error) {
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
