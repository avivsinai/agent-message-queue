//go:build !windows

package claude

import (
	"os"
	"syscall"
)

// openSessionsDir opens the sessions directory for listing in one atomic
// step that refuses anything but a directory: O_DIRECTORY fails on a FIFO
// or file, O_NOFOLLOW on a symlink, and O_NONBLOCK keeps a FIFO swapped in
// from blocking the open (codex #858 r2: a plain os.Open on a FIFO waited
// for a writer and froze discovery).
func openSessionsDir(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}
