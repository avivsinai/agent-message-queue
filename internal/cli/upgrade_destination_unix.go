//go:build unix

package cli

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func upgradeDestinationWritable(path string) error {
	if err := unix.Access(path, unix.W_OK); err != nil {
		return &os.PathError{Op: "access", Path: path, Err: err}
	}
	dir := filepath.Dir(path)
	if err := unix.Access(dir, unix.W_OK); err != nil {
		return &os.PathError{Op: "access", Path: dir, Err: err}
	}
	return nil
}
