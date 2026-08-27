//go:build !darwin && !linux

package selfupgrade

import "os"

type imageFileIdentity struct {
	Device    uint64
	Inode     uint64
	CTimeSec  int64
	CTimeNsec int64
	FileInfo  os.FileInfo
}

func sameImageFileIdentity(a, b os.FileInfo) bool { return os.SameFile(a, b) }

func captureImageFileIdentity(info os.FileInfo) (imageFileIdentity, bool) {
	return imageFileIdentity{FileInfo: info}, info != nil
}
