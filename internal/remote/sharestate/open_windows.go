//go:build windows

package sharestate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// winDir is the key directory reached one component at a time with
// NtCreateFile relative to the previous handle, each opened with
// FILE_OPEN_REPARSE_POINT: a component that is a link, or becomes one after
// its parent was verified, is opened as the link itself and refused. Leaves
// open relative to the final handle the same way, so no full path is ever
// resolved again (codex slice 1 review r3 #1, r4: holding a directory does
// not stop FSCTL_SET_REPARSE_POINT turning it into a junction in place).
type winDir struct {
	path string
	h    windows.Handle
}

const shareAll = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE

// openKeyDir opens root (trusted, by path), then each part relative to the
// previous handle as a real directory.
func openKeyDir(root string, parts []string) (dirHandle, error) {
	p, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE, shareAll, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: root, Err: err}
	}
	path := root
	for _, part := range parts {
		path = filepath.Join(path, part)
		next, err := openAt(h, part, path, windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE, windows.FILE_DIRECTORY_FILE)
		_ = windows.CloseHandle(h)
		if err != nil {
			return nil, err
		}
		if err := refuseReparse(next, path, "is not a real directory"); err != nil {
			_ = windows.CloseHandle(next)
			return nil, err
		}
		h = next
	}
	return &winDir{path: path, h: h}, nil
}

// openLeaf opens name relative to the directory handle, no-follow;
// readLeaf checks the handle is a regular file.
func (d *winDir) openLeaf(name string) (*os.File, error) {
	path := filepath.Join(d.path, name)
	h, err := openAt(d.h, name, path, windows.FILE_GENERIC_READ, windows.FILE_NON_DIRECTORY_FILE)
	if err != nil {
		return nil, err
	}
	if err := refuseReparse(h, path, "is a link"); err != nil {
		_ = windows.CloseHandle(h)
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}

func (d *winDir) Close() error { return windows.CloseHandle(d.h) }

// openAt opens name relative to dir with FILE_OPEN_REPARSE_POINT.
func openAt(dir windows.Handle, name, path string, access, options uint32) (windows.Handle, error) {
	un, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return windows.InvalidHandle, err
	}
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: dir, ObjectName: un, Attributes: windows.OBJ_CASE_INSENSITIVE}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var h windows.Handle
	var iosb windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&h, access, &oa, &iosb, nil, 0, shareAll, windows.FILE_OPEN,
		options|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	if err != nil {
		var st windows.NTStatus
		if errors.As(err, &st) {
			err = st.Errno()
		}
		return windows.InvalidHandle, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return h, nil
}

// refuseReparse refuses a handle that is a reparse point (a link or
// junction opened as itself).
func refuseReparse(h windows.Handle, path, what string) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return &os.PathError{Op: "stat", Path: path, Err: err}
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("%s %s; refusing", path, what)
	}
	return nil
}
