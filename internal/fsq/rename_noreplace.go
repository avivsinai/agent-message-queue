//go:build linux || darwin

package fsq

import "path/filepath"

// renameNoReplace publishes oldPath onto newPath without replacing an existing
// destination. Directories are opened through the pinned os.Root so an ambient
// alias of Base cannot redirect the rename.
func (r *DeliveryRoot) renameNoReplace(oldPath, newPath string) error {
	oldDir, err := r.root.Open(filepath.Dir(oldPath))
	if err != nil {
		return err
	}
	defer func() { _ = oldDir.Close() }()

	newDir, err := r.root.Open(filepath.Dir(newPath))
	if err != nil {
		return err
	}
	defer func() { _ = newDir.Close() }()

	return renameatNoReplace(
		int(oldDir.Fd()),
		filepath.Base(oldPath),
		int(newDir.Fd()),
		filepath.Base(newPath),
	)
}
