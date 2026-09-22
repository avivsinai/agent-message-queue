//go:build windows

package fsq

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// isDirSyncUnsupported reports whether err is the class of failure a
// directory flush can produce on a filesystem that does not support it.
// Exact twin of the Unix isSyncUnsupported set (EINVAL / ENOTSUP) — NOTHING
// broader. In Windows errno terms that is ERROR_INVALID_FUNCTION (the
// EINVAL twin) and ERROR_NOT_SUPPORTED / ERROR_CALL_NOT_IMPLEMENTED (the
// ENOTSUP twins). ERROR_ACCESS_DENIED is deliberately NOT tolerated: on
// NTFS it is a real failure and must surface (architect ruling on bead
// u35, v3 2026-09-22).
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

// ntOpenDirectoryIn opens leafName (a single component) relative to the
// already-open parent directory handle, as a writable directory handle:
// NtCreateFile with OBJECT_ATTRIBUTES.RootDirectory — the same pinned-root
// pattern claim_rename_windows.go and direct_child_rename_windows.go use.
// FILE_GENERIC_WRITE is required because FlushFileBuffers demands
// GENERIC_WRITE (MSDN); FILE_DIRECTORY_FILE|FILE_OPEN_FOR_BACKUP_INTENT|
// FILE_SYNCHRONOUS_IO_NONALERT make it a proper directory open.
func ntOpenDirectoryIn(parent windows.Handle, leafName string) (windows.Handle, error) {
	// A zero-length (but non-null) ObjectName with a RootDirectory refers
	// to the RootDirectory object itself — the NT idiom for reopening the
	// same directory with different access rights (the root case); a
	// non-empty leafName resolves relative to the parent. A NULL ObjectName
	// pointer is NOT the same thing: NT rejects it with
	// STATUS_OBJECT_NAME_INVALID when RootDirectory is set.
	objectName, err := windows.NewNTUnicodeString(leafName)
	if err != nil {
		return 0, fmt.Errorf("dir sync name %q: %w", leafName, err)
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: parent,
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	var status windows.IO_STATUS_BLOCK
	var handle windows.Handle
	ntStatus := windows.NtCreateFile(
		&handle,
		windows.FILE_GENERIC_WRITE|windows.SYNCHRONIZE,
		attributes,
		&status,
		nil,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0, 0,
	)
	if ntStatus != nil {
		return 0, ntStatus
	}
	return handle, nil
}

// flushDirHandle flushes and closes a directory handle, applying the
// unsupported tolerance.
func flushDirHandle(handle windows.Handle, name string) error {
	if err := windows.FlushFileBuffers(handle); err != nil {
		closeErr := windows.CloseHandle(handle)
		if isDirSyncUnsupported(err) {
			return nil
		}
		if closeErr != nil {
			return fmt.Errorf("flush dir %s: %w (close: %v)", name, err, closeErr)
		}
		return fmt.Errorf("flush dir %s: %w", name, err)
	}
	return windows.CloseHandle(handle)
}

// syncDirPlatform flushes a directory's metadata to stable storage on
// Windows: open the directory relative to the pinned root (parent via
// root.Open, leaf via NtCreateFile with RootDirectory — no ambient path is
// ever resolved) and FlushFileBuffers the FILE_GENERIC_WRITE handle. This
// is the mechanism that makes the NT rename-information-class renames
// (maildir claim, no-replace publication, renameDirectChildNoReplace,
// DeliveryRoot.WriteFileAtomic) durable — those classes have no
// write-through flag — and it establishes the directory chain in ADR
// Addendum 4 (bead u35, ruling v3 2026-09-22).
func (r *DeliveryRoot) syncDirPlatform(dir string) error {
	clean := filepath.ToSlash(filepath.Clean(dir))
	if clean == "." || clean == "/" || clean == "" {
		// The root itself: Go's os.Root opens "." read-only (GENERIC_READ)
		// and FlushFileBuffers requires GENERIC_WRITE, so obtain a writable
		// handle by reopening "." with FILE_GENERIC_WRITE relative to the
		// root's own handle. No ambient path is involved. If the kernel
		// refuses the relative reopen, surface the NTSTATUS text and errno
		// rather than falling back to an ambient open silently.
		self, err := r.root.Open(".")
		if err != nil {
			return fmt.Errorf("open pinned root for sync: %w", err)
		}
		defer func() { _ = self.Close() }()
		handle, ntErr := ntOpenDirectoryIn(windows.Handle(self.Fd()), "")
		if ntErr != nil {
			return fmt.Errorf("reopen pinned root for sync: %w (%v)", ntErr, windowsClaimError(ntErr))
		}
		return flushDirHandle(handle, "root")
	}
	if strings.HasPrefix(clean, "../") || clean == ".." {
		return fmt.Errorf("dir sync %s escapes the pinned root", dir)
	}
	parent, err := r.root.Open(filepath.Dir(clean))
	if err != nil {
		return fmt.Errorf("open parent of %s for sync: %w", dir, err)
	}
	defer func() { _ = parent.Close() }()
	handle, ntErr := ntOpenDirectoryIn(windows.Handle(parent.Fd()), filepath.Base(clean))
	if ntErr != nil {
		// No missing-directory no-op: the Unix twin propagates the open
		// error, and a flush that reports durable for a chain that does
		// not exist would falsify ADR Addendum 4 on exactly the path it
		// protects (ruling 10:05Z). Map NTSTATUS through windowsClaimError
		// for a readable errno in the message.
		return fmt.Errorf("open dir %s relative to root for sync: %w (%v)", dir, ntErr, windowsClaimError(ntErr))
	}
	return flushDirHandle(handle, dir)
}

// syncDirPlatformAmbient is the ambient-path form used by the package-level
// WriteFileAtomic (which holds no DeliveryRoot); it opens by absolute path —
// the only form available there. No DeliveryRoot path reaches it.
func syncDirPlatformAmbient(dir string) error {
	namePtr, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return fmt.Errorf("dir sync path %s: %w", dir, err)
	}
	handle, err := windows.CreateFile(namePtr,
		windows.GENERIC_READ|windows.GENERIC_WRITE, // FlushFileBuffers requires GENERIC_WRITE (MSDN)
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
	return flushDirHandle(handle, dir)
}
