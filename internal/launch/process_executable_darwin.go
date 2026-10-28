//go:build darwin

package launch

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	darwinProcPIDPathInfo        = 11
	darwinProcPIDPathInfoMaxSize = 4 * unix.PathMax
)

// ProcessExecutable is the kernel's executable path for process pid.
func ProcessExecutable(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("invalid process id %d", pid)
	}
	var buffer [darwinProcPIDPathInfoMaxSize]byte
	_, _, errno := unix.Syscall6(darwinSysProcInfo, darwinProcInfoCallPIDInfo, uintptr(pid),
		darwinProcPIDPathInfo, 0, uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)))
	runtime.KeepAlive(&buffer)
	if errno != 0 {
		return "", fmt.Errorf("proc_pidpath pid %d: %w", pid, errno)
	}
	end := bytes.IndexByte(buffer[:], 0)
	if end <= 0 {
		return "", fmt.Errorf("proc_pidpath pid %d returned no terminated path", pid)
	}
	path := string(buffer[:end])
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", fmt.Errorf("proc_pidpath pid %d returned a non-canonical absolute path", pid)
	}
	return path, nil
}

// ProcessArgs reads argv from the kernel without interpreting a process title.
func ProcessArgs(pid int) ([]string, error) {
	data, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, err
	}
	if len(data) < 4 {
		return nil, fmt.Errorf("process %d argument count is unavailable", pid)
	}
	argc := int(binary.LittleEndian.Uint32(data[:4]))
	if argc <= 0 || argc > len(data) {
		return nil, fmt.Errorf("process %d argument count is invalid", pid)
	}
	data = data[4:]
	end := bytes.IndexByte(data, 0)
	if end < 0 {
		return nil, fmt.Errorf("process %d executable path is unterminated", pid)
	}
	data = bytes.TrimLeft(data[end+1:], "\x00")
	args := make([]string, 0, argc)
	for len(args) < argc {
		end := bytes.IndexByte(data, 0)
		if end < 0 {
			return nil, fmt.Errorf("process %d arguments are incomplete", pid)
		}
		args = append(args, string(data[:end]))
		data = data[end+1:]
	}
	return args, nil
}
