//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package registry

import (
	"fmt"
	"os"
)

const lockNoFollow = 0

func validateRegistryDir(dir string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("registry directory %q must not be a symlink", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("registry directory %q must be a directory", dir)
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("registry directory %q must be mode 0700", dir)
	}
	return nil
}

func recheckLockIdentity(_ *os.File, path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("registry lock %q must be a regular file", path)
	}
	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf("registry lock %q must be mode 0600", path)
	}
	return nil
}
