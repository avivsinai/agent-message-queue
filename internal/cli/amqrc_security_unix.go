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
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if ok && uint32(st.Uid) != uint32(os.Getuid()) {
		return fmt.Errorf(".amqrc at %s is owned by uid %d, want uid %d", path, st.Uid, os.Getuid())
	}
	return nil
}
