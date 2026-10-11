//go:build linux

package fsq

import "golang.org/x/sys/unix"

// renameDirectChildNoReplace publishes a staged directory. When the
// filesystem refuses RENAME_NOREPLACE (issue #1034), a check-then-rename is
// not enough: rename replaces an empty directory, and a peer that creates
// the same operator-chosen name with mkdir inside that window would share
// this tree while both report an exclusive create. So the fallback first
// claims the name with an exclusive mkdir, then renames over its own empty
// placeholder. A peer mkdir now gets EEXIST, and a placeholder that someone
// fills fails the rename with ENOTEMPTY.
func (r *DeliveryRoot) renameDirectChildNoReplace(oldName, newName string) error {
	dir, err := r.root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	fd := int(dir.Fd())
	err = renameat2(fd, oldName, fd, newName, unix.RENAME_NOREPLACE)
	if err == nil || !isRenameFlagUnsupported(err) {
		return err
	}
	if err := unix.Mkdirat(fd, newName, 0o700); err != nil {
		return err
	}
	if err := unix.Renameat(fd, oldName, fd, newName); err != nil {
		_ = unix.Unlinkat(fd, newName, unix.AT_REMOVEDIR)
		return err
	}
	return nil
}
