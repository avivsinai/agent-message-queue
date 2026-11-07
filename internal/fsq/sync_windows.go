//go:build windows

package fsq

// SyncDir flushes a directory's metadata to stable storage. The test hook
// intercepts; otherwise the real implementation in sync_windows_impl.go runs.
func SyncDir(dir string) error {
	if packageSyncDirFaultForTest != nil {
		return packageSyncDirFaultForTest(dir)
	}
	return syncDirPlatformAmbient(dir)
}
