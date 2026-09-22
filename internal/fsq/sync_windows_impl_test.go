//go:build windows

package fsq

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// TestSyncDirPlatformFlushesRealDirectory runs on windows-latest CI (the
// ^TestSyncDirPlatform entry in the windows-claim-test -run list, bead u35
// ruling v3): syncDirPlatform must take the flush path on a real directory,
// opened relative to the pinned root — never by ambient path.
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
	// Root-relative multi-component path, as syncDirChain passes them.
	if err := r.syncDirPlatform(filepath.Join("bridge", "transfer-ledger", "s1")); err != nil {
		t.Fatalf("syncDirPlatform on a real directory: %v", err)
	}
	// A single-component sync.
	if err := r.syncDirPlatform("bridge"); err != nil {
		t.Fatalf("syncDirPlatform single component: %v", err)
	}
	// A missing directory propagates the open error (Unix twin; no no-op —
	// ruling 10:05Z: a flush that reports durable for a chain that does not
	// exist would falsify Addendum 4).
	if err := r.syncDirPlatform(filepath.Join("bridge", "does-not-exist")); err == nil {
		t.Fatal("syncDirPlatform accepted a missing directory")
	}
	// The root directory itself syncs through the pinned-handle chain, not
	// an ambient open.
	if err := r.syncDirPlatform("."); err != nil {
		t.Fatalf("syncDirPlatform on the pinned root: %v", err)
	}
	// An escaping path is refused, never resolved.
	if err := r.syncDirPlatform(filepath.Join("..", "outside")); err == nil {
		t.Fatal("syncDirPlatform accepted an escaping path")
	}
	// The ambient form (package WriteFileAtomic) still flushes.
	if err := syncDirPlatformAmbient(dir); err != nil {
		t.Fatalf("syncDirPlatformAmbient on a real directory: %v", err)
	}
}

// TestSyncDirPlatformUnsupportedTolerated pins the exact twin of the Unix
// isSyncUnsupported set (ruling v3, condition 1): only the documented
// not-supported errnos are tolerated; ERROR_ACCESS_DENIED is a real failure
// and must surface.
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
}
