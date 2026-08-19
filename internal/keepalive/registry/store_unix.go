//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package registry

import (
	"fmt"
	"os"
	"syscall"
)

const lockNoFollow = syscall.O_NOFOLLOW

func validateRegistryDir(dir string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("registry directory %q must not be a symlink", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("registry directory %q must be a directory", dir)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("registry directory %q has unavailable ownership metadata", dir)
	}
	if uint32(stat.Uid) != uint32(os.Geteuid()) {
		return fmt.Errorf("registry directory %q is owned by uid %d, want current uid %d", dir, stat.Uid, os.Geteuid())
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("registry directory %q must be mode 0700", dir)
	}
	return nil
}

func recheckLockIdentity(lock *os.File, path string) error {
	var fdStat syscall.Stat_t
	if err := syscall.Fstat(int(lock.Fd()), &fdStat); err != nil {
		return err
	}
	var pathStat syscall.Stat_t
	if err := syscall.Lstat(path, &pathStat); err != nil {
		return err
	}
	if fdStat.Dev != pathStat.Dev || fdStat.Ino != pathStat.Ino {
		return fmt.Errorf("registry lock %q was replaced", path)
	}
	if fdStat.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return fmt.Errorf("registry lock %q is not a regular file", path)
	}
	if fdStat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("registry lock %q is owned by uid %d, want current uid %d", path, fdStat.Uid, os.Geteuid())
	}
	if fdStat.Mode&0o777 != 0o600 {
		return fmt.Errorf("registry lock %q must be mode 0600", path)
	}
	return nil
}
