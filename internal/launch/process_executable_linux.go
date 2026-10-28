//go:build linux

package launch

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// ProcessExecutable is the kernel's executable path for process pid.
func ProcessExecutable(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("invalid process id %d", pid)
	}
	return os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
}

// ProcessArgs reads argv from the kernel without interpreting a process title.
func ProcessArgs(pid int) ([]string, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("invalid process id %d", pid)
	}
	file, err := os.Open(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxProcessArgsBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxProcessArgsBytes {
		return nil, fmt.Errorf("process %d arguments exceed the safety limit", pid)
	}
	parts := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
	args := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			args = append(args, part)
		}
	}
	return args, nil
}
