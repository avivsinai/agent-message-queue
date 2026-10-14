//go:build darwin

package xferv3

import (
	"os"

	"golang.org/x/sys/unix"
)

func fullSync(f *os.File) error {
	_, err := unix.FcntlInt(f.Fd(), unix.F_FULLFSYNC, 0)
	return err
}
