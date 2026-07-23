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

func captureWakeFileIdentity(info os.FileInfo) (wakeFileIdentity, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return wakeFileIdentity{}, false
	}
	return wakeFileIdentity{
		Device:    uint64(stat.Dev),
		Inode:     uint64(stat.Ino),
		CTimeSec:  int64(stat.Ctim.Sec),
		CTimeNsec: int64(stat.Ctim.Nsec),
	}, true
}

func matchesWakeFileIdentity(identity wakeFileIdentity, info os.FileInfo) bool {
	current, ok := captureWakeFileIdentity(info)
	return ok && identity == current
}
