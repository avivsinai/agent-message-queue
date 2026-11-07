//go:build darwin || linux

package wakelock

import (
	"os"
	"syscall"
)

// CurrentUID reports the effective uid; false means the platform cannot tell.
func CurrentUID() (int, bool) { return os.Geteuid(), true }

// FileOwnerUID reports the owner uid of info; false means the platform cannot
// tell.
func FileOwnerUID(info os.FileInfo) (int, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(stat.Uid), true
}
