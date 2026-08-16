//go:build linux

package fsq

import "golang.org/x/sys/unix"

func (r *DeliveryRoot) renameDirectChildNoReplace(oldName, newName string) error {
	dir, err := r.root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return unix.Renameat2(int(dir.Fd()), oldName, int(dir.Fd()), newName, unix.RENAME_NOREPLACE)
}
