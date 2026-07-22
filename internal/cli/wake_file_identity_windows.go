//go:build windows

package cli

import "os"

func sameWakeFileIdentity(a, b os.FileInfo) bool { return os.SameFile(a, b) }

func captureWakeFileIdentity(info os.FileInfo) (wakeFileIdentity, bool) {
	return wakeFileIdentity{fileInfo: info}, info != nil
}

func matchesWakeFileIdentity(identity wakeFileIdentity, info os.FileInfo) bool {
	return identity.fileInfo != nil && info != nil && os.SameFile(identity.fileInfo, info)
}
