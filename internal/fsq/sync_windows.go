//go:build windows

package fsq

// syncDirAmbientForTest swaps the implementation behind the ambient-path
// SyncDir calls; nil means the no-op platform sync. Windows syncs are no-ops,
// so the hook only matters for tests that count or fault calls.
var syncDirAmbientForTest func(dir string) error

func syncDirAmbientSwapForTest(fn func(dir string) error) (restore func()) {
	prev := syncDirAmbientForTest
	syncDirAmbientForTest = fn
	return func() { syncDirAmbientForTest = prev }
}

// SyncDir is a no-op on Windows.
func SyncDir(dir string) error {
	if syncDirAmbientForTest != nil {
		return syncDirAmbientForTest(dir)
	}
	_ = dir
	return nil
}
