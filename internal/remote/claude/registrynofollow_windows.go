//go:build windows

package claude

// registryNoFollow is zero on Windows: os.O_NOFOLLOW does not exist there
// and Windows symlink traversal at open needs FILE_FLAG_OPEN_REPARSE_POINT
// via CreateFile, which os.OpenFile does not expose. The lstat gate plus
// the LimitReader bound carry the r3 P2-2 hardening on this platform.
const registryNoFollow = 0
