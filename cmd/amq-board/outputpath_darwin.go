package main

import (
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// launchd opens the log before exec. F_GETPATH identifies that open file
// even when the project no longer resolves to the store used at install.
func outputFilePath(f *os.File) string {
	var path [unix.PathMax]byte
	// F_GETPATH takes a pointer-sized argument through libSystem's fcntl.
	// Pin the output buffer across the integer-argument wrapper.
	var pin runtime.Pinner
	pin.Pin(&path[0])
	defer pin.Unpin()
	_, err := unix.FcntlInt(f.Fd(), unix.F_GETPATH, int(uintptr(unsafe.Pointer(&path[0]))))
	if err != nil {
		return ""
	}
	return unix.ByteSliceToString(path[:])
}
