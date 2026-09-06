//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTreeRelationUsesPhysicalIdentity(t *testing.T) {
	realRoot := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	if got := relateTrees(realRoot, alias); got != TreeRelationSame {
		t.Fatalf("relateTrees(real, alias) = %v, want Same", got)
	}
	if got := relateTrees(realRoot, t.TempDir()); got != TreeRelationDifferent {
		t.Fatalf("relateTrees(distinct roots) = %v, want Different", got)
	}
	if got := relateTrees(realRoot, filepath.Join(t.TempDir(), "missing")); got != TreeRelationUnknown {
		t.Fatalf("relateTrees(real, missing) = %v, want Unknown", got)
	}
}
