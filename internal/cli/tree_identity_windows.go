//go:build windows

package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/windows"
)

const treeIdentityPlatform = "windows"

func platformTreeIdentityToken(path string, _ os.FileInfo) (string, error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	handle, err := windows.CreateFile(
		pathPtr,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(handle)

	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return "", err
	}
	return fmt.Sprintf("v1:windows:%x:%x:%x", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow), nil
}

func validPlatformTreeIdentityToken(token string) bool {
	parts := strings.Split(token, ":")
	if len(parts) != 5 || parts[0] != "v1" || parts[1] != treeIdentityPlatform {
		return false
	}
	for _, part := range parts[2:] {
		if _, err := strconv.ParseUint(part, 16, 32); err != nil {
			return false
		}
	}
	return true
}
