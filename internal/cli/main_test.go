package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
)

const (
	cliHelperEnv                 = "AMQ_TEST_CLI_HELPER"
	wakeRestartPTYOwnerHelperEnv = "AMQ_TEST_WAKE_RESTART_PTY_OWNER"
	wakeSIGPIPEHelperEnv         = "AMQ_TEST_WAKE_SIGPIPE_HELPER"
	wakeSIGPIPERootEnv           = "AMQ_TEST_WAKE_SIGPIPE_ROOT"
)

var cliSecureTempRoot string

// TestMain gives the cli package a hermetic environment so the developer's shell
// can't leak routing context into tests. The cross-tree send guard (issue #144)
// keys off AM_ROOT / AM_BASE_ROOT; without this, running the suite from inside a
// coop session (where those are set) would make tests that pass --root to a temp
// dir look like refused cross-tree sends. Tests that need these signals set them
// explicitly via t.Setenv, which overrides and restores around this clean baseline.
func TestMain(m *testing.M) {
	readTIOCSTILegacySysctl = func() ([]byte, error) {
		return nil, os.ErrNotExist
	}
	if os.Getenv(wakeSIGPIPEHelperEnv) == "1" {
		ignoredBefore := signal.Ignored(syscall.SIGPIPE)
		gate := os.NewFile(3, "wake-sigpipe-gate")
		ready := os.NewFile(4, "wake-sigpipe-ready")
		helperDone := errors.New("wake SIGPIPE helper complete")
		err := runWakeWithLoop(
			[]string{
				"--root", os.Getenv(wakeSIGPIPERootEnv),
				"--me", "codex",
				"--inject-mode", wakeInjectModeNone,
				"--interrupt=false",
			},
			func(wakeConfig) error {
				if _, err := ready.Write([]byte{1}); err != nil {
					return fmt.Errorf("signal SIGPIPE test readiness: %w", err)
				}
				if _, err := io.ReadFull(gate, make([]byte, 1)); err != nil {
					return fmt.Errorf("await closed diagnostic pipe: %w", err)
				}
				_, _ = fmt.Fprintln(os.Stderr, "broken-pipe-probe")
				_, _ = fmt.Fprintln(os.Stdout, "survived")
				return helperDone
			},
		)
		if !errors.Is(err, helperDone) {
			_, _ = fmt.Fprintln(os.Stdout, err)
			os.Exit(1)
		}
		if ignoredAfter := signal.Ignored(syscall.SIGPIPE); ignoredAfter != ignoredBefore {
			_, _ = fmt.Fprintf(os.Stdout, "SIGPIPE ignored state after wake = %t, want baseline %t\n", ignoredAfter, ignoredBefore)
			os.Exit(1)
		}
		probeReader, probeWriter, pipeErr := os.Pipe()
		if pipeErr != nil {
			_, _ = fmt.Fprintln(os.Stdout, pipeErr)
			os.Exit(1)
		}
		_ = probeReader.Close()
		_, pipeErr = probeWriter.Write([]byte{1})
		_ = probeWriter.Close()
		if !errors.Is(pipeErr, syscall.EPIPE) {
			_, _ = fmt.Fprintf(os.Stdout, "post-wake broken pipe error = %v, want EPIPE\n", pipeErr)
			os.Exit(1)
		}
		os.Exit(0)
	}

	if os.Getenv(cliHelperEnv) == "1" {
		if err := Run(os.Args[1:], "test"); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(GetExitCode(err))
		}
		os.Exit(0)
	}
	if os.Getenv(injectViaHelperEnv) == "1" || os.Getenv(wakeRestartPTYOwnerHelperEnv) == "1" ||
		os.Getenv("AMQ_TEST_WAKE_RESTART_BOUND_EXEC") != "" {
		os.Exit(m.Run())
	}

	for _, k := range []string{"AM_ROOT", "AM_ROOT_ID", "AM_BASE_ROOT", "AM_BASE_ROOT_ID", "AM_SESSION", "AMQ_GLOBAL_ROOT", envWakeOwner, "AMQ_WAKE_PRIVATE_STOP_FD"} {
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

func TestTIOCSTILegacySysctlTestDefaultIsNotReadableZero(t *testing.T) {
	data, err := readTIOCSTILegacySysctl()
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("default error = %v, want os.ErrNotExist", err)
	}
	if len(data) != 0 {
		t.Fatalf("default data = %q, want empty", data)
	}
}
