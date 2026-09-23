//go:build !windows

package sharestate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// unixDir is a directory file descriptor reached by openat, one component
// at a time, from the root.
type unixDir struct{ f *os.File }

// openKeyDir opens root, then each part with O_DIRECTORY|O_NOFOLLOW
// relative to the previous handle: a symlinked or replaced component fails
// the open instead of being followed, and the returned handle stays on the
// directory that was verified.
func openKeyDir(root string, parts []string) (dirHandle, error) {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: root, Err: err}
	}
	path := root
	for _, part := range parts {
		path = filepath.Join(path, part)
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return nil, fmt.Errorf("%s is not a real directory; refusing", path)
		}
		if err != nil {
			return nil, &os.PathError{Op: "open", Path: path, Err: err}
		}
		fd = next
	}
	return unixDir{os.NewFile(uintptr(fd), path)}, nil
}

// openLeaf opens name in the directory without following a symlink and
// without blocking on a FIFO.
func (d unixDir) openLeaf(name string) (*os.File, error) {
	path := filepath.Join(d.f.Name(), name)
	fd, err := unix.Openat(int(d.f.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ELOOP) {
		return nil, fmt.Errorf("%s is a symlink; refusing", path)
	}
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

func (d unixDir) Close() error { return d.f.Close() }
