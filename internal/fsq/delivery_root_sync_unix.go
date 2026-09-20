//go:build !windows

package fsq

// packageSyncDirFaultForTest, when set, applies to every DeliveryRoot opened
// in this process — including roots the test never sees directly (e.g. a
// courier's internally opened handle). Instance-level SetSyncDirFaultForTest
// takes precedence.
var packageSyncDirFaultForTest func(dir string) error

func (r *DeliveryRoot) syncDir(dir string) error {
	if r.syncDirForTest != nil {
		return r.syncDirForTest(dir)
	}
	if packageSyncDirFaultForTest != nil {
		return packageSyncDirFaultForTest(dir)
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
