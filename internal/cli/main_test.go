package cli

import (
	"os"
	"testing"
)

// TestMain gives the cli package a hermetic environment so the developer's shell
// can't leak routing context into tests. The cross-tree send guard (issue #144)
// keys off AM_ROOT / AM_BASE_ROOT; without this, running the suite from inside a
// coop session (where those are set) would make tests that pass --root to a temp
// dir look like refused cross-tree sends. Tests that need these signals set them
// explicitly via t.Setenv, which overrides and restores around this clean baseline.
func TestMain(m *testing.M) {
	for _, k := range []string{"AM_ROOT", "AM_BASE_ROOT", "AMQ_GLOBAL_ROOT"} {
		_ = os.Unsetenv(k)
	}
	os.Exit(m.Run())
}
