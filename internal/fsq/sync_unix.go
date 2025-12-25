//go:build !windows

package fsq

import (
	"errors"
	"os"
	"syscall"
)

// SyncDir fsyncs a directory to ensure directory entries are durable.
func SyncDir(dir string) error {
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
