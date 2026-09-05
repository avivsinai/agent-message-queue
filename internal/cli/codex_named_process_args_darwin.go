//go:build darwin

package cli

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func readCodexProcessArgs(pid int) ([]string, error) {
	data, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, err
	}
	if len(data) < 4 || len(data) > codexNamedMaxToolOutput {
		return nil, errors.New("process arguments have an invalid size")
	}
	argc := int(binary.LittleEndian.Uint32(data[:4]))
	if argc <= 0 || argc > 4096 {
		return nil, fmt.Errorf("process argument count %d is invalid", argc)
	}
	data = data[4:]
	end := bytes.IndexByte(data, 0)
	if end < 0 {
		return nil, errors.New("process executable path is unterminated")
	}
	data = bytes.TrimLeft(data[end+1:], "\x00")
	args := make([]string, 0, argc)
	for len(args) < argc && len(data) > 0 {
		end = bytes.IndexByte(data, 0)
		if end < 0 {
			return nil, errors.New("process argument is unterminated")
		}
		args = append(args, string(data[:end]))
		data = data[end+1:]
	}
	if len(args) != argc {
		return nil, errors.New("process argument vector is incomplete")
	}
	return args, nil
}
