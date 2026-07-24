//go:build !windows

package fsq

func (r *DeliveryRoot) syncDir(dir string) error {
	if r.syncDirForTest != nil {
		return r.syncDirForTest(dir)
	}
	return r.syncDirPlatform(dir)
}

func (r *DeliveryRoot) syncDirPlatform(dir string) error {
	file, err := r.root.Open(dir)
	if err != nil {
		return err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil {
		if isSyncUnsupported(syncErr) {
			return nil
		}
		return syncErr
	}
	return closeErr
}
