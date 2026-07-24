//go:build windows

package fsq

func (r *DeliveryRoot) syncDir(dir string) error {
	if r.syncDirForTest != nil {
		return r.syncDirForTest(dir)
	}
	return r.syncDirPlatform(dir)
}

func (r *DeliveryRoot) syncDirPlatform(_ string) error {
	return nil
}
