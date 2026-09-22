//go:build windows

package fsq

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// isDirSyncUnsupported reports whether err is the class of failure a
// directory flush can produce on a filesystem that does not support it.
// Exact twin of the Unix isSyncUnsupported set (EINVAL / ENOTSUP) — NOTHING
// broader. In Windows errno terms that is ERROR_INVALID_FUNCTION (the
// EINVAL twin) and ERROR_NOT_SUPPORTED / ERROR_CALL_NOT_IMPLEMENTED (the
// ENOTSUP twins). ERROR_ACCESS_DENIED is deliberately NOT tolerated: on
// NTFS it is a real failure (wrong handle, ACL denial) and must surface
// (architect ruling on bead u35, 2026-09-22).
func isDirSyncUnsupported(err error) bool {
	if err == nil {
		return false
	}
	var errno windows.Errno
	if errors.As(err, &errno) {
		switch errno {
		case windows.ERROR_INVALID_FUNCTION, windows.ERROR_NOT_SUPPORTED,
			windows.ERROR_CALL_NOT_IMPLEMENTED:
			return true
		}
	}
	return false
}

// syncDirPlatform flushes a directory's metadata to stable storage on
// Windows: open a handle with FILE_FLAG_BACKUP_SEMANTICS (the documented
// way to obtain a directory handle) and FlushFileBuffers it. This is the
// supported equivalent of fsync(dir); without it the directory-entry
// durability that the bridge transfer ledger and the maildir renames rely
// on (ADR Addendum 4) does not hold (bead u35).
func (r *DeliveryRoot) syncDirPlatform(dir string) error {
	// The injected root (r.root) must resolve relative paths; open through
	// it so a test-scoped root is honored.
	full := filepath.Join(r.root.Name(), filepath.FromSlash(dir))
	namePtr, err := windows.UTF16PtrFromString(full)
	if err != nil {
		return fmt.Errorf("dir sync path %s: %w", full, err)
	}
	handle, err := windows.CreateFile(namePtr,
		windows.GENERIC_READ, // FlushFileBuffers requires read access on the handle
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS, // directory open (WRITE_THROUGH unnecessary: flush is the sync point)
		0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return nil // nothing to sync
		}
		return fmt.Errorf("open dir %s for sync: %w", full, err)
	}
	if err := windows.FlushFileBuffers(handle); err != nil {
		closeErr := windows.CloseHandle(handle)
		if isDirSyncUnsupported(err) {
			return nil
		}
		if closeErr != nil {
			return fmt.Errorf("flush dir %s: %w (close: %v)", full, err, closeErr)
		}
		return fmt.Errorf("flush dir %s: %w", full, err)
	}
	return windows.CloseHandle(handle)
}

// SyncDir flushes a directory by absolute or root-relative path (ambient
// form used by WriteFileAtomic, which holds no DeliveryRoot). The test hook
// lives in sync_windows.go.
func syncDirPlatformAmbient(dir string) error {
	namePtr, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return fmt.Errorf("dir sync path %s: %w", dir, err)
	}
	handle, err := windows.CreateFile(namePtr,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return nil
		}
		return fmt.Errorf("open dir %s for sync: %w", dir, err)
	}
	if err := windows.FlushFileBuffers(handle); err != nil {
		closeErr := windows.CloseHandle(handle)
		if isDirSyncUnsupported(err) {
			return nil
		}
		if closeErr != nil {
			return fmt.Errorf("flush dir %s: %w (close: %v)", dir, err, closeErr)
		}
		return fmt.Errorf("flush dir %s: %w", dir, err)
	}
	return windows.CloseHandle(handle)
}
