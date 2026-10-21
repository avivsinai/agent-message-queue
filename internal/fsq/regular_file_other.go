//go:build !unix

package fsq

import "os"

func openRegularNoFollow(path string) (*os.File, error) {
	return os.Open(path)
}

// openedIsLstatFile requires the opened file to be the one the lstat gate
// saw: this open follows links, so identity is the only swap check.
func openedIsLstatFile(lstat, opened os.FileInfo) bool { return os.SameFile(lstat, opened) }
