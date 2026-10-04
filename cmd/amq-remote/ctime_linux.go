//go:build linux

package main

import (
	"os"
	"syscall"
	"time"
)

// ctimeOf returns the inode change time (when the file was written by the
// install) from the syscall stat record.
func ctimeOf(info os.FileInfo) time.Time {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}
	}
	return time.Unix(st.Ctim.Sec, st.Ctim.Nsec)
}
