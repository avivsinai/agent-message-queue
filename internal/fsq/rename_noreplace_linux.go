//go:build linux

package fsq

import (
	"errors"

	"golang.org/x/sys/unix"
)

// renameat2 is a var so a test can stand in for a filesystem that refuses
// RENAME_NOREPLACE (issue #1034); production always calls unix.Renameat2.
var renameat2 = unix.Renameat2

// renameatNoReplace renames without replacing an existing destination. Some
// filesystems (FUSE servers without rename flags, old kernels) refuse
// RENAME_NOREPLACE while a plain rename works. There the guard degrades to
// check-then-rename: a destination created between the check and the rename
// is replaced. Message and staging names are unique, so only the abnormal
// external reintroduction that ClaimCollisionError describes can hit that
// window. A genuinely invalid rename fails again on the plain rename, so the
// fallback does not hide a real EINVAL.
func renameatNoReplace(oldDirFD int, oldName string, newDirFD int, newName string) error {
	err := renameat2(oldDirFD, oldName, newDirFD, newName, unix.RENAME_NOREPLACE)
	if err == nil || !isRenameFlagUnsupported(err) {
		return err
	}
	var st unix.Stat_t
	switch statErr := unix.Fstatat(newDirFD, newName, &st, unix.AT_SYMLINK_NOFOLLOW); {
	case statErr == nil:
		return unix.EEXIST
	case !errors.Is(statErr, unix.ENOENT):
		return statErr
	}
	return unix.Renameat(oldDirFD, oldName, newDirFD, newName)
}

func isRenameFlagUnsupported(err error) bool {
	return errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.ENOTSUP)
}
