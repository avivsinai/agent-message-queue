//go:build windows

package fsq

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

func (r *DeliveryRoot) syncDirPlatform(_ string) error {
	return nil
}
