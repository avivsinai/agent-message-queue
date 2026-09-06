//go:build darwin || linux

package fsq

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenDeliveryRootRejectsSwapBetweenAuthorizationAndOpen(t *testing.T) {
	parent := t.TempDir()
	base := filepath.Join(parent, "authorized")
	parked := filepath.Join(parent, "authorized-parked")
	outside := filepath.Join(parent, "outside")
	for _, path := range []string{base, outside} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatalf("MkdirAll(%s): %v", path, err)
		}
	}
	identity, err := SnapshotDeliveryRoot(base)
	if err != nil {
		t.Fatalf("SnapshotDeliveryRoot: %v", err)
	}
	if err := os.Rename(base, parked); err != nil {
		t.Fatalf("park authorized root: %v", err)
	}
	if err := os.Symlink(outside, base); err != nil {
		t.Fatalf("install malicious alias: %v", err)
	}

	root, err := OpenDeliveryRoot(base, identity)
	if root != nil {
		_ = root.Close()
		t.Fatal("OpenDeliveryRoot returned a capability for the swapped root")
	}
	if err == nil || !errors.Is(err, ErrDeliveryRootChanged) || !strings.Contains(err.Error(), "between authorization and capability open") {
		t.Fatalf("OpenDeliveryRoot error = %v, want authorization/open mismatch", err)
	}
}
