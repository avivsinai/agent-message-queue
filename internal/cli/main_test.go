package cli

import (
	"fmt"
	"os"
	"testing"
)

const cliHelperEnv = "AMQ_TEST_CLI_HELPER"

// TestMain gives the cli package a hermetic environment so the developer's shell
// can't leak routing context into tests. The cross-tree send guard (issue #144)
// keys off AM_ROOT / AM_BASE_ROOT; without this, running the suite from inside a
// coop session (where those are set) would make tests that pass --root to a temp
// dir look like refused cross-tree sends. Tests that need these signals set them
// explicitly via t.Setenv, which overrides and restores around this clean baseline.
func TestMain(m *testing.M) {
	if os.Getenv(cliHelperEnv) == "1" {
		if err := Run(os.Args[1:], "test"); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(GetExitCode(err))
		}
		os.Exit(0)
	}

	for _, k := range []string{"AM_ROOT", "AM_ROOT_ID", "AM_BASE_ROOT", "AM_BASE_ROOT_ID", "AM_SESSION", "AMQ_GLOBAL_ROOT"} {
		_ = os.Unsetenv(k)
	}
	os.Exit(m.Run())
}
