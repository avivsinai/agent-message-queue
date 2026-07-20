//go:build !windows

package cli

import (
	"fmt"
	"os"
	"syscall"
)

// validateAmqrcFile enforces that project configuration is owned by the
// invoking user. Permission bits are intentionally not enforced here: older
// configs commonly use 0644 and the file contains no secrets.
func validateAmqrcFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf(".amqrc at %s is a symlink", path)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf(".amqrc at %s has unavailable ownership metadata", path)
	}
	if uint32(st.Uid) != uint32(os.Geteuid()) {
		return fmt.Errorf(".amqrc at %s is owned by uid %d, want euid %d", path, st.Uid, os.Geteuid())
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf(".amqrc at %s is group/world-writable", path)
	}
	return nil
}
