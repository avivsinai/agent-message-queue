//go:build linux

package cli

import (
	"errors"
	"io"
	"os"
	"strconv"
)

func readCodexProcessArgs(pid int) ([]string, error) {
	file, err := os.Open("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, codexNamedMaxToolOutput+1))
	if err != nil {
		return nil, err
	}
	if len(data) > codexNamedMaxToolOutput {
		return nil, errors.New("process arguments exceed the safety limit")
	}
	return splitProcCmdline(data), nil
}
