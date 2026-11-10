//go:build !darwin && !linux && !windows

package fsq

func (c *layoutDirCapability) syncPlatform() error {
	err := c.file.Sync()
	if isSyncUnsupported(err) {
		return nil
	}
	return err
}
