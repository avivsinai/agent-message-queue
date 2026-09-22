//go:build !windows

package fsq

// publishReplaceDurable is the Unix twin of the Windows MoveFileEx
// WRITE_THROUGH path: plain pinned-root rename (rename(2) durability comes
// from the directory syncs that syncDirPlatform performs).
func (r *DeliveryRoot) publishReplaceDurable(tmpPath, finalPath string) error {
	return r.root.Rename(tmpPath, finalPath)
}
