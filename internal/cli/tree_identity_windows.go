//go:build windows

package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"unsafe"

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

	// FILE_ID_INFO is required for ReFS: the legacy 64-bit file index can
	// collide. Unsupported/partial filesystems remain unverifiable.
	type fileIDInfo struct {
		VolumeSerialNumber uint64
		FileID             [16]byte
	}
	var info fileIDInfo
	if err := windows.GetFileInformationByHandleEx(handle, windows.FileIdInfo, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return "", err
	}
	return fmt.Sprintf("v1:windows:%x:%x", info.VolumeSerialNumber, info.FileID), nil
}

func validPlatformTreeIdentityToken(token string) bool {
	parts := strings.Split(token, ":")
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != treeIdentityPlatform {
		return false
	}
	if _, err := strconv.ParseUint(parts[2], 16, 64); err != nil {
		return false
	}
	if len(parts[3]) != 32 {
		return false
	}
	_, err := strconv.ParseUint(parts[3][:16], 16, 64)
	if err != nil {
		return false
	}
	_, err = strconv.ParseUint(parts[3][16:], 16, 64)
	return err == nil
}
