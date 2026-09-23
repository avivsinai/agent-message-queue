//go:build windows

package sharestate

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// winDir holds an open handle on every directory from the root down to the
// key directory. Each is opened without following a reparse point and
// without FILE_SHARE_DELETE, so while the handles are held no component can
// be renamed, removed or swapped for a link; a leaf opened by path then
// resolves through exactly the directories that were verified (codex slice
// 1 review r3 #1: os.Root resolves a link swapped in between its checks).
type winDir struct {
	path    string
	handles []windows.Handle
}

const (
	shareNoDelete = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE
	noFollow      = windows.FILE_FLAG_OPEN_REPARSE_POINT
)

// openKeyDir opens root, then each part under it, as held real directories.
func openKeyDir(root string, parts []string) (dirHandle, error) {
	d := &winDir{path: root}
	if err := d.hold(root); err != nil {
		return nil, err
	}
	for _, part := range parts {
		d.path = filepath.Join(d.path, part)
		if err := d.hold(d.path); err != nil {
			_ = d.Close()
			return nil, err
		}
	}
	return d, nil
}

// hold opens path as a directory, no-follow, and keeps the handle.
func (d *winDir) hold(path string) error {
	h, err := openNoFollow(path, windows.FILE_FLAG_BACKUP_SEMANTICS)
	if err != nil {
		return err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		_ = windows.CloseHandle(h)
		return &os.PathError{Op: "stat", Path: path, Err: err}
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		_ = windows.CloseHandle(h)
		return fmt.Errorf("%s is not a real directory; refusing", path)
	}
	d.handles = append(d.handles, h)
	return nil
}

// openLeaf opens name in the held directory without following a reparse
// point; readLeaf checks the handle is a regular file.
func (d *winDir) openLeaf(name string) (*os.File, error) {
	path := filepath.Join(d.path, name)
	h, err := openNoFollow(path, 0)
	if err != nil {
		return nil, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		_ = windows.CloseHandle(h)
		return nil, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(h)
		return nil, fmt.Errorf("%s is a link; refusing", path)
	}
	return os.NewFile(uintptr(h), path), nil
}

func (d *winDir) Close() error {
	for i := len(d.handles) - 1; i >= 0; i-- {
		_ = windows.CloseHandle(d.handles[i])
	}
	d.handles = nil
	return nil
}

func openNoFollow(path string, flags uint32) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, shareNoDelete, nil, windows.OPEN_EXISTING, flags|noFollow, 0)
	if err != nil {
		return windows.InvalidHandle, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return h, nil
}
