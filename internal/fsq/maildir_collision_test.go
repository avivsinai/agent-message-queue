package fsq

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolvePublishCollisionTmpCleanupFailureIsCommittedDurabilityError
// (review-827-r3, codex r2-r3 finding 2): when a collision resolves to a
// destination that ALREADY holds exactly these bytes, the publication is a
// fact. A failed tmp cleanup must be classified as
// *CommittedDurabilityError (published, dest-dir sync short-circuited) —
// never as a plain error a caller would treat as a proven non-delivery and
// re-apply.
func TestResolvePublishCollisionTmpCleanupFailureIsCommittedDurabilityError(t *testing.T) {
	base := t.TempDir()
	if err := EnsureAgentDirs(base, "alice"); err != nil {
		t.Fatal(err)
	}
	identity, err := SnapshotDeliveryRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	root, err := OpenDeliveryRoot(base, identity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })

	const payload = "collision-payload"
	fn := "m-cde-collision.md"
	newPath := filepath.Join("agents", "alice", "inbox", "new", fn)

	// Pre-place matching bytes at the destination: the rename will report
	// ErrExist, the read will prove the bytes match, and the tmp Remove will
	// be forced to fail.
	if err := root.CreateExclusiveFile(newPath, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	// There is no Remove fault hook; use a NON-EMPTY DIRECTORY as the tmp
	// path: Remove fails deterministically with ENOTEMPTY (not IsNotExist).
	tmpDir := filepath.Join("agents", "alice", "inbox", "tmp", "forced-collision-tmp")
	if err := root.root.MkdirAll(tmpDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := root.root.MkdirAll(filepath.Join(tmpDir, "child"), 0o700); err != nil {
		t.Fatal(err)
	}

	err = root.resolvePublishCollision(tmpDir, newPath, []byte(payload), os.ErrExist)
	if err == nil {
		t.Fatal("resolvePublishCollision = nil, want CommittedDurabilityError for failed tmp cleanup")
	}
	var cde *CommittedDurabilityError
	if !errors.As(err, &cde) {
		t.Fatalf("err = %v (%T), want *CommittedDurabilityError", err, err)
	}
	if !strings.Contains(cde.Error(), "tmp cleanup failed") {
		t.Fatalf("err = %v, want tmp-cleanup cause preserved", err)
	}
	if cde.FinalPath == "" || !strings.Contains(cde.FinalPath, fn) {
		t.Fatalf("FinalPath = %q, want the destination path", cde.FinalPath)
	}
}
