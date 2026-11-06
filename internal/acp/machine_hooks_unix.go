//go:build unix

package acp

import (
	"os"
	"strconv"
	"syscall"
)

// fileIdentity is the device and inode of the file info describes.
func fileIdentity(info os.FileInfo) string {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return strconv.FormatUint(uint64(st.Dev), 10) + "." + strconv.FormatUint(st.Ino, 10)
	}
	return "?"
}
