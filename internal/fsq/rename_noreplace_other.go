//go:build !linux && !darwin

package fsq

// renameNoReplace falls back to os.Root.Rename on platforms without a nested
// no-replace primitive. Windows claims use claimRename instead of this path.
func (r *DeliveryRoot) renameNoReplace(oldPath, newPath string) error {
	return r.root.Rename(oldPath, newPath)
}
