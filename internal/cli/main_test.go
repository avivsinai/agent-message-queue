package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/update"
)

const (
	cliHelperEnv                 = "AMQ_TEST_CLI_HELPER"
	wakeRestartPTYOwnerHelperEnv = "AMQ_TEST_WAKE_RESTART_PTY_OWNER"
)

var cliSecureTempRoot string

// cliTestPackageDir is this package's source directory, resolved from the
// test source file itself rather than from cwd. TestMain moves the process cwd
// to the secure temp root (issue #707), so tests that need the repository
// (building ./cmd/amq, launchapi goldens, git history) resolve it from here.
var cliTestPackageDir = func() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("resolve cli test package directory: runtime.Caller failed")
	}
	return filepath.Dir(file)
}()

// cliTestRepoRoot returns the absolute repository root for tests that build or
// read repository files. It does not depend on the process cwd.
func cliTestRepoRoot() (string, error) {
	return filepath.Abs(filepath.Join(cliTestPackageDir, "..", ".."))
}

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
	guardTestProcessReplacement()

	for _, k := range []string{"AM_ROOT", "AM_ROOT_ID", "AM_BASE_ROOT", "AM_BASE_ROOT_ID", "AM_SESSION", "AMQ_GLOBAL_ROOT", envWakeOwner, "AMQ_WAKE_PRIVATE_STOP_FD"} {
		_ = os.Unsetenv(k)
	}

	fakeHome, err := os.MkdirTemp("", "amq-cli-test-home-")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "create isolated test home: %v\n", err)
		os.Exit(1)
	}
	if err := os.Chmod(fakeHome, 0o700); err != nil {
		_ = os.RemoveAll(fakeHome)
		_, _ = fmt.Fprintf(os.Stderr, "secure isolated test home: %v\n", err)
		os.Exit(1)
	}
	// Pin the Go toolchain caches to the real home's locations BEFORE the home
	// override: GOPATH/GOCACHE default under $HOME, and a module cache inside
	// the disposable test home both slows the run (re-downloads) and breaks
	// cleanup (mod cache files are read-only). Env-set values win unchanged.
	realHome, realHomeErr := os.UserHomeDir()
	realGopath := os.Getenv("GOPATH")
	if realGopath == "" && realHomeErr == nil && realHome != "" {
		realGopath = filepath.Join(realHome, "go")
	}
	realGomodcache := os.Getenv("GOMODCACHE")
	if realGomodcache == "" && realGopath != "" {
		realGomodcache = filepath.Join(realGopath, "pkg", "mod")
	}
	realGocache := os.Getenv("GOCACHE")
	if realGocache == "" && realHomeErr == nil && realHome != "" {
		if userCache, uerr := os.UserCacheDir(); uerr == nil {
			realGocache = filepath.Join(userCache, "go-build")
		}
	}
	// Override every variable os.UserHomeDir (and the tools it fronts) consults
	// so tests can never read or write the operator's real home: ~/.amqrc must
	// not leak authority into root resolution (issue #988: with a live ~/.amqrc
	// pointing at ~/.agent-mail, TestSetup*/TestRunEnvJSON*/TestCoopExec*/
	// TestEnvAndBareSend* failed identically on main), and nothing may write
	// through to the live ~/.agent-mail. Per-test t.Setenv("HOME", ...) keeps
	// working: it overrides and restores around this baseline. HOMEDRIVE and
	// HOMEPATH are set for Windows callers and tools that resolve the home from
	// the drive-letter pair rather than USERPROFILE.
	for _, env := range []struct{ key, value string }{
		{"HOME", fakeHome},
		{"USERPROFILE", fakeHome},
		{"HOMEDRIVE", filepath.VolumeName(fakeHome)},
		{"HOMEPATH", strings.TrimPrefix(fakeHome, filepath.VolumeName(fakeHome))},
		{"GOPATH", realGopath},
		{"GOMODCACHE", realGomodcache},
		{"GOCACHE", realGocache},
	} {
		if err := os.Setenv(env.key, env.value); err != nil {
			_ = os.RemoveAll(fakeHome)
			_, _ = fmt.Fprintf(os.Stderr, "set %s for tests: %v\n", env.key, err)
			os.Exit(1)
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		_ = os.RemoveAll(fakeHome)
		_, _ = fmt.Fprintf(os.Stderr, "resolve test home directory: %v\n", err)
		os.Exit(1)
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		_ = os.RemoveAll(fakeHome)
		_, _ = fmt.Fprintf(os.Stderr, "resolve test home directory symlinks: %v\n", err)
		os.Exit(1)
	}
	tempRoot, err := os.MkdirTemp(home, ".amq-cli-test-")
	if err != nil {
		_ = os.RemoveAll(fakeHome)
		_, _ = fmt.Fprintf(os.Stderr, "create secure test temp root: %v\n", err)
		os.Exit(1)
	}
	if err := os.Chmod(tempRoot, 0o700); err != nil {
		_ = os.RemoveAll(tempRoot)
		_ = os.RemoveAll(fakeHome)
		_, _ = fmt.Fprintf(os.Stderr, "secure test temp root: %v\n", err)
		os.Exit(1)
	}

	cliSecureTempRoot = tempRoot
	workingDir, err := os.Getwd()
	if err != nil {
		_ = os.RemoveAll(tempRoot)
		_ = os.RemoveAll(fakeHome)
		_, _ = fmt.Fprintf(os.Stderr, "resolve test working directory: %v\n", err)
		os.Exit(1)
	}
	// Run every test from the secure temp root, not the package directory.
	// The cwd-local routing guard walks up from cwd to the Git top looking for
	// an initialized .agent-mail; on a developer checkout that finds the live
	// queue, and every test that pins a temp root then fails with "active
	// root ... conflicts with initialized repo-local root ... detected from
	// cwd" (issue #707). CI never sees this because its checkout has no queue.
	// The temp root sits directly under the FAKE home, which the walk treats as
	// global state rather than repo-local evidence; a developer whose real HOME
	// is a Git worktree (dotfiles kept as a repo in HOME) is therefore also
	// covered, because tests never see the real home at all.
	if err := os.Chdir(tempRoot); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "isolate test working directory: %v\n", err)
		_ = os.RemoveAll(tempRoot)
		_ = os.RemoveAll(fakeHome)
		os.Exit(1)
	}

	// Isolate the update-check cache (issue #646 class) so tests that drive
	// runUpgrade / the update Notifier never write through to the developer's
	// real ~/Library/Caches/amq. AMQ_CACHE_DIR is the authoritative override
	// (darwin's os.UserCacheDir ignores HOME); XDG_CACHE_HOME covers any code
	// path that consults it directly.
	testCacheDir := filepath.Join(tempRoot, "cache")
	if err := os.MkdirAll(testCacheDir, 0o700); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "create test cache dir: %v\n", err)
		os.Exit(1)
	}
	for _, env := range []struct {
		key   string
		value string
	}{
		{key: update.EnvCacheDir, value: testCacheDir},
		{key: "XDG_CACHE_HOME", value: testCacheDir},
	} {
		if err := os.Setenv(env.key, env.value); err != nil {
			_ = os.RemoveAll(tempRoot)
			_ = os.RemoveAll(fakeHome)
			_, _ = fmt.Fprintf(os.Stderr, "set %s for tests: %v\n", env.key, err)
			os.Exit(1)
		}
	}

	exitCode := m.Run()
	if err := os.Chdir(workingDir); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "restore test working directory: %v\n", err)
		exitCode = 1
	}
	cliSecureTempRoot = ""
	if err := os.RemoveAll(tempRoot); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "remove secure test temp root: %v\n", err)
		exitCode = 1
	}
	if err := os.RemoveAll(fakeHome); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "remove isolated test home: %v\n", err)
		exitCode = 1
	}
	os.Exit(exitCode)
}
