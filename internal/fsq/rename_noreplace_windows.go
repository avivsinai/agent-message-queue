//go:build windows

package fsq

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// renameNoReplace publishes oldPath onto newPath without replacing an
// existing destination. os.Root.Rename requests replacement semantics on
// Windows, so publication uses the same exclusive NT disposition primitive
// as the Windows claim path.
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

	err = renameWindowsNoReplace(
		windows.Handle(oldDir.Fd()), filepath.Base(oldPath),
		windows.Handle(newDir.Fd()), filepath.Base(newPath),
	)
	if errors.Is(err, windows.STATUS_OBJECT_NAME_COLLISION) ||
		errors.Is(err, windows.ERROR_ALREADY_EXISTS) ||
		errors.Is(err, windows.ERROR_FILE_EXISTS) {
		return os.ErrExist
	}
	return windowsClaimError(err)
}
