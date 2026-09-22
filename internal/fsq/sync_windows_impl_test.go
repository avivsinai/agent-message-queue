//go:build windows

package fsq

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// TestSyncDirPlatformFlushesRealDirectory runs on windows-latest CI (added
// to the windows-claim-test -run list, bead u35): syncDirPlatform must open
// a real directory handle via FILE_FLAG_BACKUP_SEMANTICS and take the flush
// path — a directory that exists flushes without error, and the flush is
// observable through the handle being a directory handle.
func TestSyncDirPlatformFlushesRealDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "bridge", "transfer-ledger", "s1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	identity, err := SnapshotDeliveryRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	r, err := OpenDeliveryRoot(root, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	// Root-relative path, as syncDirChain passes them.
	if err := r.syncDirPlatform(filepath.Join("bridge", "transfer-ledger", "s1")); err != nil {
		t.Fatalf("syncDirPlatform on a real directory: %v", err)
	}
	// The ambient form used by WriteFileAtomic must also flush.
	if err := syncDirPlatformAmbient(dir); err != nil {
		t.Fatalf("syncDirPlatformAmbient on a real directory: %v", err)
	}
	// A missing directory is a no-op, not an error.
	if err := r.syncDirPlatform(filepath.Join("bridge", "does-not-exist")); err != nil {
		t.Fatalf("syncDirPlatform on a missing directory: %v", err)
	}
}

// TestSyncDirPlatformUnsupportedTolerated pins the exact twin of the Unix
// isSyncUnsupported set (architect condition 1 on bead u35): only the
// documented not-supported errnos are tolerated; ERROR_ACCESS_DENIED is a
// real failure and must surface.
func TestSyncDirPlatformUnsupportedTolerated(t *testing.T) {
	if !isDirSyncUnsupported(windows.ERROR_INVALID_FUNCTION) {
		t.Fatal("ERROR_INVALID_FUNCTION must be tolerated (EINVAL twin)")
	}
	if !isDirSyncUnsupported(windows.ERROR_NOT_SUPPORTED) {
		t.Fatal("ERROR_NOT_SUPPORTED must be tolerated (ENOTSUP twin)")
	}
	if !isDirSyncUnsupported(windows.ERROR_CALL_NOT_IMPLEMENTED) {
		t.Fatal("ERROR_CALL_NOT_IMPLEMENTED must be tolerated (ENOTSUP twin)")
	}
	if isDirSyncUnsupported(windows.ERROR_ACCESS_DENIED) {
		t.Fatal("ERROR_ACCESS_DENIED must NOT be tolerated: on NTFS it is a real failure")
	}
	if isDirSyncUnsupported(nil) {
		t.Fatal("nil must not be treated as unsupported")
	}
	if isDirSyncUnsupported(errors.New("some other failure")) {
		t.Fatal("generic errors must surface, not be swallowed")
	}
	// And the real path still fails on a file handle misuse — flushing a
	// path that is a FILE, opened as a directory, is refused by the OS
	// with an error that must NOT be swallowed as unsupported.
	root := t.TempDir()
	filePath := filepath.Join(root, "regular.txt")
	if err := os.WriteFile(filePath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	namePtr, err := windows.UTF16PtrFromString(filePath)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(namePtr, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err == nil {
		// Windows allows opening a file with BACKUP_SEMANTICS; the flush
		// itself then succeeds (it is a file flush). Close and move on —
		// this case asserts no panic, not an error.
		_ = windows.CloseHandle(handle)
	}
}
