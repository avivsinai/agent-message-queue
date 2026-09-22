//go:build !windows

package fsq

import (
	"errors"
	"os"
	"syscall"
)

// syncDirAmbientForTest swaps the implementation behind the ambient-path
// SyncDir calls. nil restores the platform sync. It returns the restore func.
// Existing in-package tests may also reach the variable directly.
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

// SyncDir fsyncs a directory to ensure directory entries are durable.
func SyncDir(dir string) error {
	if syncDirAmbientForTest != nil {
		return syncDirAmbientForTest(dir)
	}
	file, err := os.Open(dir)
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

func isSyncUnsupported(err error) bool {
	return errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP)
}
