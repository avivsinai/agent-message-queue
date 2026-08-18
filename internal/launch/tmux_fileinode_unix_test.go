//go:build unix

package launch

import (
	"os"
	"syscall"
)

func fileInode(info os.FileInfo) (uint64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return stat.Ino, true
}
