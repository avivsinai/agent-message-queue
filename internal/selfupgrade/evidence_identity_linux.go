//go:build linux

package selfupgrade

import (
	"os"
	"syscall"
)

type imageFileIdentity struct {
	Device    uint64
	Inode     uint64
	CTimeSec  int64
	CTimeNsec int64
}

func sameImageFileIdentity(a, b os.FileInfo) bool {
	sa, aok := captureImageFileIdentity(a)
	sb, bok := captureImageFileIdentity(b)
	return aok && bok && sa == sb
}

func captureImageFileIdentity(info os.FileInfo) (imageFileIdentity, bool) {
	if info == nil {
		return imageFileIdentity{}, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return imageFileIdentity{}, false
	}
	return imageFileIdentity{
		Device:    uint64(stat.Dev),
		Inode:     uint64(stat.Ino),
		CTimeSec:  int64(stat.Ctim.Sec),
		CTimeNsec: int64(stat.Ctim.Nsec),
	}, true
}
