//go:build darwin

package selfupgrade

import (
	"os"
	"syscall"
)

type imageFileIdentity struct {
	Device    uint64
	Inode     uint64
	Mode      uint16
	UID       uint32
	MTimeSec  int64
	MTimeNsec int64
	CTimeSec  int64
	CTimeNsec int64
}

// sameImageFileIdentity ignores ctime: Darwin stages restart images as hard
// links to the installed file, so any wake or package manager that links or
// unlinks a name for the same inode changes its ctime. Every content write
// moves mtime, so mtime, mode, and owner carry the change proof. A same-UID
// writer that resets mtime with utimes is outside the threat model, as for
// the staged pathname exec.
func sameImageFileIdentity(a, b os.FileInfo) bool {
	sa, aok := captureImageFileIdentity(a)
	sb, bok := captureImageFileIdentity(b)
	if !aok || !bok {
		return false
	}
	sa.CTimeSec, sa.CTimeNsec = sb.CTimeSec, sb.CTimeNsec
	return sa == sb
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
		Mode:      stat.Mode,
		UID:       stat.Uid,
		MTimeSec:  int64(stat.Mtimespec.Sec),
		MTimeNsec: int64(stat.Mtimespec.Nsec),
		CTimeSec:  int64(stat.Ctimespec.Sec),
		CTimeNsec: int64(stat.Ctimespec.Nsec),
	}, true
}
