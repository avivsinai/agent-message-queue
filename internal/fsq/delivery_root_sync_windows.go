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

// syncDirPlatform is implemented in sync_windows_impl.go (bead u35:
// FlushFileBuffers on a FILE_FLAG_BACKUP_SEMANTICS directory handle).
