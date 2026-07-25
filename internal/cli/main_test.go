package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

const cliHelperEnv = "AMQ_TEST_CLI_HELPER"

var cliSecureTempRoot string

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
	if os.Getenv(injectViaHelperEnv) == "1" {
		os.Exit(m.Run())
	}

	for _, k := range []string{"AM_ROOT", "AM_ROOT_ID", "AM_BASE_ROOT", "AM_BASE_ROOT_ID", "AM_SESSION", "AMQ_GLOBAL_ROOT"} {
		_ = os.Unsetenv(k)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "resolve test home directory: %v\n", err)
		os.Exit(1)
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "resolve test home directory symlinks: %v\n", err)
		os.Exit(1)
	}
	tempRoot, err := os.MkdirTemp(home, ".amq-cli-test-")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "create secure test temp root: %v\n", err)
		os.Exit(1)
	}
	if err := os.Chmod(tempRoot, 0o700); err != nil {
		_ = os.RemoveAll(tempRoot)
		_, _ = fmt.Fprintf(os.Stderr, "secure test temp root: %v\n", err)
		os.Exit(1)
	}

	cliSecureTempRoot = tempRoot
	exitCode := m.Run()
	cliSecureTempRoot = ""
	if err := os.RemoveAll(tempRoot); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "remove secure test temp root: %v\n", err)
		exitCode = 1
	}
	os.Exit(exitCode)
}
