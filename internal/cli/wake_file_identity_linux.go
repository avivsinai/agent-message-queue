//go:build linux

package cli

import (
	"os"
	"syscall"
)

func sameWakeFileIdentity(a, b os.FileInfo) bool {
	sa, oa := a.Sys().(*syscall.Stat_t)
	sb, ob := b.Sys().(*syscall.Stat_t)
	return oa && ob && sa.Dev == sb.Dev && sa.Ino == sb.Ino && sa.Ctim == sb.Ctim
}
