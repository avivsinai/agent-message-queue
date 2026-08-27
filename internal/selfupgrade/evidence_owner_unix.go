//go:build darwin || linux

package selfupgrade

import (
	"os"
	"syscall"
)

func imageFileOwnerUID(info os.FileInfo) (int, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(stat.Uid), true
}

func imageCurrentUID() (int, bool) { return os.Geteuid(), true }
