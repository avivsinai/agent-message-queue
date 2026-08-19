//go:build windows

package fsq

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

type fileRenameInformationEx struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

const fileRenameInformationExClass = 65

// renameDirectChildNoReplace uses FileRenameInformationEx without
// FILE_RENAME_REPLACE_IF_EXISTS. os.Root.Rename cannot be used here because
// Go's Windows implementation explicitly requests replacement semantics.
func (r *DeliveryRoot) renameDirectChildNoReplace(oldName, newName string) error {
	dir, err := r.root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	dirHandle := windows.Handle(dir.Fd())
	return renameWindowsNoReplace(dirHandle, oldName, dirHandle, newName)
}

func renameWindowsNoReplace(oldDir windows.Handle, oldName string, newDir windows.Handle, newName string) error {
	objectName, err := windows.NewNTUnicodeString(oldName)
	if err != nil {
		return err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), RootDirectory: oldDir,
		ObjectName: objectName, Attributes: windows.OBJ_CASE_INSENSITIVE,
	}
	var source windows.Handle
	var openStatus windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(
		&source, windows.SYNCHRONIZE|windows.DELETE, attributes, &openStatus, nil, 0,
		windows.FILE_SHARE_DELETE|windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		windows.FILE_OPEN,
		windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0, 0,
	)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(source) }()

	name, err := windows.UTF16FromString(newName)
	if err != nil {
		return err
	}
	name = name[:len(name)-1]
	headerSize := unsafe.Offsetof(fileRenameInformationEx{}.FileName)
	buffer := make([]byte, int(headerSize)+len(name)*2)
	info := (*fileRenameInformationEx)(unsafe.Pointer(&buffer[0]))
	info.Flags = windows.FILE_RENAME_POSIX_SEMANTICS
	info.RootDirectory = newDir
	info.FileNameLength = uint32(len(name) * 2)
	copy(unsafe.Slice(&info.FileName[0], len(name)), name)

	var renameStatus windows.IO_STATUS_BLOCK
	return windows.NtSetInformationFile(
		source, &renameStatus, &buffer[0], uint32(len(buffer)), fileRenameInformationExClass,
	)
}
