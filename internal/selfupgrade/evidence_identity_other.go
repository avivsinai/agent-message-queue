//go:build !darwin && !linux

package selfupgrade

import "os"

type imageFileIdentity struct {
	FileInfo os.FileInfo
}

func sameImageFileIdentity(a, b os.FileInfo) bool { return os.SameFile(a, b) }

func captureImageFileIdentity(info os.FileInfo) (imageFileIdentity, bool) {
	return imageFileIdentity{FileInfo: info}, info != nil
}
