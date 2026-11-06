package fsq

import "time"

// FileVersionSettle is how old a file's last change must be before its
// version may stand for its content. A write lands in a timestamp tick that
// can be as coarse as one second, so a second write in the tick of the first
// can leave every metadata field equal; once the last change is older than
// this, any later write has a later ctime.
const FileVersionSettle = time.Second

// FileVersion is one version of a directory entry as its metadata shows it:
// device, inode, mode, size, mtime and ctime. A missing entry is a version
// too. Equal stable versions of one path mean the entry was not written,
// replaced, renamed over, or had its mode or owner changed in between: the
// ctime is set by the kernel on each of those and cannot be set back. A
// version is a hint that lets a reader skip an unchanged file; the content
// read when it changed stays the authority.
type FileVersion struct {
	exists       bool
	dev, ino     uint64
	mode         uint32
	size         int64
	mtime, ctime int64
}
