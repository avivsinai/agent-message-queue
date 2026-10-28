//go:build linux

package launch

import (
	"bytes"
	"fmt"
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
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || data[len(data)-1] != 0 {
		return nil, fmt.Errorf("process %d arguments are unavailable", pid)
	}
	return strings.Split(string(bytes.TrimSuffix(data, []byte{0})), "\x00"), nil
}
