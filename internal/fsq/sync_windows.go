//go:build windows

package fsq

// syncDirAmbientForTest swaps the implementation behind the ambient-path
// SyncDir calls; nil means the real platform sync (bead u35: FlushFileBuffers
// on a FILE_FLAG_BACKUP_SEMANTICS directory handle, not a no-op).
var syncDirAmbientForTest func(dir string) error

// SyncDirAmbientSwapForTest swaps the implementation behind the ambient
// SyncDir path for callers outside the package (fault-injection tests).
// It returns the restore func.
func SyncDirAmbientSwapForTest(fn func(dir string) error) (restore func()) {
	return syncDirAmbientSwapForTest(fn)
}

func syncDirAmbientSwapForTest(fn func(dir string) error) (restore func()) {
	prev := syncDirAmbientForTest
	syncDirAmbientForTest = fn
	return func() { syncDirAmbientForTest = prev }
}

// SyncDir flushes a directory's metadata to stable storage. The test hook
// intercepts; otherwise the real implementation in sync_windows_impl.go runs.
func SyncDir(dir string) error {
	if syncDirAmbientForTest != nil {
		return syncDirAmbientForTest(dir)
	}
	return syncDirPlatformAmbient(dir)
}
