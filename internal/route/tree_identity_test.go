//go:build !windows

package route

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

	if got := (Walker{}).RelateTrees(realRoot, alias); got != TreeRelationSame {
		t.Fatalf("(Walker{}).RelateTrees(real, alias) = %v, want Same", got)
	}
	if got := (Walker{}).RelateTrees(realRoot, t.TempDir()); got != TreeRelationDifferent {
		t.Fatalf("(Walker{}).RelateTrees(distinct roots) = %v, want Different", got)
	}
	if got := (Walker{}).RelateTrees(realRoot, filepath.Join(t.TempDir(), "missing")); got != TreeRelationUnknown {
		t.Fatalf("(Walker{}).RelateTrees(real, missing) = %v, want Unknown", got)
	}
}
