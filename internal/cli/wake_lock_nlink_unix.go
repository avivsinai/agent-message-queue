//go:build darwin || linux

package cli

import (
	"os"
	"syscall"
)

func wakeLockHasMultipleLinks(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint64(stat.Nlink) > 1
}
