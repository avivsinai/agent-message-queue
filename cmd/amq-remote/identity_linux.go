//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
)

// processIdentityOS reads a running process's executable identity on Linux
// through /proc/<pid>/exe, which resolves to the running executable's file
// even when the upgrade deleted it.
func processIdentityOS(ctx context.Context, pid int) (execIdentity, error) {
	info, err := os.Stat(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return execIdentity{}, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return execIdentity{}, errors.New("no stat identity")
	}
	return execIdentity{Dev: uint64(st.Dev), Inode: st.Ino}, nil
}
