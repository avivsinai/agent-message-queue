//go:build !windows

package binding

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// ownedByUser refuses a directory another user owns or can write: they could
// create the binding's directories themselves (docs/configuration.md).
func ownedByUser(fi os.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("has no ownership metadata")
	}
	if uint32(st.Uid) != uint32(os.Geteuid()) {
		return fmt.Errorf("is owned by uid %d, not euid %d", st.Uid, os.Geteuid())
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return errors.New("is group- or world-writable")
	}
	return nil
}
