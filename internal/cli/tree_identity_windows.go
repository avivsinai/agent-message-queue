//go:build windows

package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const treeIdentityPlatform = "windows"

func platformTreeIdentityToken(path string, _ os.FileInfo) (string, error) {
	return "", fmt.Errorf("Windows identity pinning is out of scope")
	/*
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
		if invalidWindowsIdentity(info.VolumeSerialNumber, info.FileID) {
			return "", fmt.Errorf("filesystem returned sentinel file identity")
		}
		return fmt.Sprintf("v1:windows:%x:%x", info.VolumeSerialNumber, info.FileID), nil */
}

func invalidWindowsIdentity(volume uint64, id [16]byte) bool {
	if volume == 0 || volume == ^uint64(0) {
		return true
	}
	var zero, ff [16]byte
	for i := range ff {
		ff[i] = 0xff
	}
	return id == zero || id == ff
}

func validPlatformTreeIdentityToken(token string) bool {
	parts := strings.Split(token, ":")
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != treeIdentityPlatform {
		return false
	}
	volume, err := strconv.ParseUint(parts[2], 16, 64)
	if err != nil || volume == 0 || volume == ^uint64(0) {
		return false
	}
	if len(parts[3]) != 32 {
		return false
	}
	_, err = strconv.ParseUint(parts[3][:16], 16, 64)
	if err != nil {
		return false
	}
	_, err = strconv.ParseUint(parts[3][16:], 16, 64)
	if err != nil {
		return false
	}
	var id, zero, ff [16]byte
	for i := range ff {
		ff[i] = 0xff
	}
	for i := range id {
		v, parseErr := strconv.ParseUint(parts[3][i*2:i*2+2], 16, 8)
		if parseErr != nil {
			return false
		}
		id[i] = byte(v)
	}
	return id != zero && id != ff
}
