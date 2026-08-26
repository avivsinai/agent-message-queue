//go:build darwin

package cli

import (
	"os"
	"syscall"
)

// sameWakeImageStableAcrossHash reports whether two os.FileInfo snapshots of an
// open wake image fd describe the same image across a hashing pass. On Darwin a
// hardlink or unlink on any name of the inode mutates the shared inode ctime,
// so ctime cannot be part of this stability check: an in-place write changes
// mtime/size, and a replacement changes the inode. Device, inode, size, and
// mtime are the stable identity across a hash. The captured evidence still
// records ctime (see captureWakeFileIdentity / wakeImageEvidenceV1.CTimeNS);
// only this hash-time guard ignores it.
func sameWakeImageStableAcrossHash(before, after os.FileInfo) bool {
	sa, oa := before.Sys().(*syscall.Stat_t)
	sb, ob := after.Sys().(*syscall.Stat_t)
	if !oa || !ob {
		return os.SameFile(before, after) && before.Size() == after.Size() &&
			before.ModTime().Equal(after.ModTime())
	}
	return sa.Dev == sb.Dev && sa.Ino == sb.Ino && before.Size() == after.Size() &&
		sa.Mtimespec.Sec == sb.Mtimespec.Sec && sa.Mtimespec.Nsec == sb.Mtimespec.Nsec
}
