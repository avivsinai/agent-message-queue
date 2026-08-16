//go:build darwin

package fsq

import "golang.org/x/sys/unix"

func (r *DeliveryRoot) renameDirectChildNoReplace(oldName, newName string) error {
	dir, err := r.root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return unix.RenameatxNp(int(dir.Fd()), oldName, int(dir.Fd()), newName, unix.RENAME_EXCL)
}
