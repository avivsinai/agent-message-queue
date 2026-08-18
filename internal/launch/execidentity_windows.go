//go:build windows

package launch

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func executableFileIDs(path string, _ os.FileInfo) (dev, inode, volumeID, fileID uint64, err error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("open executable for identity: %w", err)
	}
	defer windows.CloseHandle(handle)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return 0, 0, 0, 0, fmt.Errorf("executable file id: %w", err)
	}
	fileIndex := uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)
	return 0, 0, uint64(info.VolumeSerialNumber), fileIndex, nil
}
