//go:build !darwin

package main

import (
	"os"
	"runtime"
	"strconv"
)

// A Linux journal stream has no regular-file path. A redirected watch may.
func outputFilePath(f *os.File) string {
	if runtime.GOOS != "linux" {
		return ""
	}
	path, _ := os.Readlink("/proc/self/fd/" + strconv.FormatUint(uint64(f.Fd()), 10))
	return path
}
