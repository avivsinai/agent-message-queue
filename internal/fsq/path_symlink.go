package fsq

import (
	"errors"
	"fmt"
)

// ErrPathIsSymlink is returned when exclusive directory creation observes a
// symlink at a path component that must be a real directory.
var ErrPathIsSymlink = errors.New("path is a symlink")

// PathSymlinkError names the component that was a symlink.
type PathSymlinkError struct {
	Path string
}

func (e *PathSymlinkError) Error() string {
	return fmt.Sprintf("path %q is a symlink", e.Path)
}

func (e *PathSymlinkError) Unwrap() error {
	return ErrPathIsSymlink
}
