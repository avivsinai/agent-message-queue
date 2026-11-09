//go:build darwin || linux

package fsq

import (
	"time"

	"golang.org/x/sys/unix"
)

// LstatVersion is the version of path, read without following a final
// symlink. stable is false when the entry changed within FileVersionSettle
// of now, so the caller must read it. A missing path is the stable absent
// version.
func LstatVersion(path string, now time.Time) (v FileVersion, stable bool, err error) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		if err == unix.ENOENT {
			return FileVersion{}, true, nil
		}
		return FileVersion{}, false, err
	}
	return versionOf(&st, now)
}

// VersionAt is LstatVersion for name in the directory open as dirfd.
func VersionAt(dirfd int, name string, now time.Time) (v FileVersion, stable bool, err error) {
	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if err == unix.ENOENT {
			return FileVersion{}, true, nil
		}
		return FileVersion{}, false, err
	}
	return versionOf(&st, now)
}

// settled reports whether a version whose last change is at mtime and ctime
// (unix nanoseconds) may stand for its content at now.
func settled(mtime, ctime int64, now time.Time) bool {
	last := max(mtime, ctime)
	return now.UnixNano()-last >= int64(FileVersionSettle)
}

func versionOf(st *unix.Stat_t, now time.Time) (FileVersion, bool, error) {
	v := FileVersion{
		exists: true,
		dev:    uint64(st.Dev),
		ino:    uint64(st.Ino),
		mode:   uint32(st.Mode),
		size:   st.Size,
		mtime:  st.Mtim.Nano(),
		ctime:  st.Ctim.Nano(),
	}
	return v, settled(v.mtime, v.ctime, now), nil
}

// StatVersion is LstatVersion of the entry path resolves to, following
// symlinks, for a reader that follows them too.
func StatVersion(path string, now time.Time) (v FileVersion, stable bool, err error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		if err == unix.ENOENT {
			return FileVersion{}, true, nil
		}
		return FileVersion{}, false, err
	}
	return versionOf(&st, now)
}
