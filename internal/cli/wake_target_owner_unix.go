//go:build darwin || linux

package cli

import (
	"os"
	"syscall"
)

var (
	wakeTargetCurrentUID   = func() (int, bool) { return os.Geteuid(), true }
	wakeTargetFileOwnerUID = func(info os.FileInfo) (int, bool) {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return 0, false
		}
		return int(stat.Uid), true
	}
)
