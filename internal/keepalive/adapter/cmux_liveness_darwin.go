//go:build darwin

package adapter

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// darwinProcessStateZombie is SZOMB from <sys/proc.h>: the process has exited
// and is waiting for its parent to reap it. A zombie no longer holds its
// controlling terminal in any meaningful sense, so it must not count as a live
// tty owner. This mirrors the precedent in internal/cli/wake_process_darwin.go;
// it is reimplemented here to avoid importing package cli.
const darwinProcessStateZombie int8 = 5

// ttyLiveOwnerCount reports how many live (non-zombie) processes hold the device
// at devPath as their controlling terminal. The kernel can only answer "how
// many live processes claim this tty", never "which cmux surface owns it", so
// callers use this count to prove a contested tty has exactly zero or at least
// one live session, not to identify the owner.
func ttyLiveOwnerCount(devPath string) (int, error) {
	info, err := os.Stat(devPath)
	if err != nil {
		return 0, fmt.Errorf("stat tty %q: %w", devPath, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return 0, fmt.Errorf("tty %q is missing device metadata", devPath)
	}
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.tty", int(st.Rdev))
	if err != nil {
		return 0, fmt.Errorf("sysctl kern.proc.tty for %q: %w", devPath, err)
	}
	count := 0
	for i := range procs {
		if procs[i].Proc.P_stat == darwinProcessStateZombie {
			continue
		}
		count++
	}
	return count, nil
}
