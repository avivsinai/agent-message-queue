//go:build darwin || linux

package wakelock

import (
	"os"
	"syscall"
)

// CurrentUID and FileOwnerUID report the effective uid and a file owner uid;
// false means the platform cannot tell.
var (
	CurrentUID   = func() (int, bool) { return os.Geteuid(), true }
	FileOwnerUID = func(info os.FileInfo) (int, bool) {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return 0, false
		}
		return int(stat.Uid), true
	}
)
