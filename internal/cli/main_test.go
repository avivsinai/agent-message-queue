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
	cliHelperEnv = "AMQ_TEST_CLI_HELPER"
	// cliTestWalkCeilingEnv carries the isolated home's walk ceiling from the
	// parent test process into helper subprocesses (os.Args[0] re-execs),
	// which start with walkCeilingForTests empty.
	cliTestWalkCeilingEnv        = "AMQ_TEST_WALK_CEILING"
	wakeRestartPTYOwnerHelperEnv = "AMQ_TEST_WAKE_RESTART_PTY_OWNER"
)

var cliSecureTempRoot string

// testRealHome is the operator's real home as resolved by TestMain before
// the override; the home-isolation regression test compares against it.
var testRealHome string

// cliTestPackageDir is this package's source directory, resolved from the
// test source file itself rather than from cwd. TestMain moves the process cwd
// to the secure temp root, so tests that need the repository (building
// ./cmd/amq, launchapi goldens, git history) resolve it from here.
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

// TestMain runs the cli package in a hermetic environment: no routing context
// from the developer's shell, and no read or write through to the operator's
// real home, temp directory, toolchain caches, or update cache. Tests that need
// a signal set it with t.Setenv, which overrides and restores around this
// baseline.
func TestMain(m *testing.M) {
	// A host's legacy TIOCSTI sysctl must not decide which injection path a
	// wake test exercises; tests that need it stub it themselves.
	readTIOCSTILegacySysctl = func() ([]byte, error) {
		return nil, os.ErrNotExist
	}
	runTestHelperProcess(m)
	guardTestProcessReplacement()
	clearRoutingEnv()

	var cleanup testMainCleanup
	fakeHome := isolateHome(&cleanup)
	tempRoot, workingDir := enterSecureTempRoot(&cleanup, fakeHome)
	isolateUpdateCache(&cleanup, tempRoot)

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

// testMainCleanup removes the directories TestMain created when setup fails
// before the run, so a failed setup leaves nothing in the real home.
type testMainCleanup struct {
	dirs []string
}

func (c *testMainCleanup) add(dir string) {
	c.dirs = append(c.dirs, dir)
}

func (c *testMainCleanup) fatalf(format string, args ...any) {
	for i := len(c.dirs) - 1; i >= 0; i-- {
		_ = os.RemoveAll(c.dirs[i])
	}
	_, _ = fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// runTestHelperProcess serves the subprocess roles this test binary re-execs
// itself as, and exits for each of them. A helper never reaches the parent's
// isolation setup; it inherits HOME and TMPDIR through the environment and
// must take the walk ceiling from it before anything resolves a root, because
// flag registration evaluates defaultRoot even with an explicit --root. A
// helper without the ceiling refuses to run rather than walk unbounded toward
// the real home.
func runTestHelperProcess(m *testing.M) {
	cliHelper := os.Getenv(cliHelperEnv) == "1"
	testHelper := os.Getenv(injectViaHelperEnv) == "1" || os.Getenv(wakeRestartPTYOwnerHelperEnv) == "1" ||
		os.Getenv("AMQ_TEST_WAKE_RESTART_BOUND_EXEC") != ""
	if !cliHelper && !testHelper {
		return
	}
	walkCeilingForTests = os.Getenv(cliTestWalkCeilingEnv)
	if walkCeilingForTests == "" {
		_, _ = fmt.Fprintln(os.Stderr, "cli test helper: "+cliTestWalkCeilingEnv+" is unset; refusing to run without the isolated walk ceiling")
		os.Exit(1)
	}
	if cliHelper {
		if err := Run(os.Args[1:], "test"); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(GetExitCode(err))
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// clearRoutingEnv drops the session and wake context of the shell that runs
// the suite. The cross-tree send guard keys off AM_ROOT and AM_BASE_ROOT, so
// from inside a coop session every test that passes --root to a temp dir
// would look like a refused cross-tree send (#144).
func clearRoutingEnv() {
	for _, k := range []string{"AM_ROOT", "AM_ROOT_ID", "AM_BASE_ROOT", "AM_BASE_ROOT_ID", "AM_SESSION", "AMQ_GLOBAL_ROOT", envWakeOwner, "AMQ_WAKE_PRIVATE_STOP_FD"} {
		_ = os.Unsetenv(k)
	}
}

// isolateHome points every home and temp variable at a private home and
// returns it. A live ~/.amqrc must not lend authority to root resolution, and
// nothing may write through to the live ~/.agent-mail (#988).
//
// The private home lives under the real home, not under os.TempDir(): the
// --inject-via ancestor check refuses group- or world-writable ancestors, and
// os.TempDir() is /tmp (mode 1777) on Linux runners. detectAgentMailDir stops
// at HOME, so the real home's own queue is never inspected.
//
// TMPDIR, TMP and TEMP point inside the private home because the cwd-ancestor
// root walk starts from t.TempDir(). With a TMPDIR inside the real home, the
// walk would climb past the private home, which it no longer recognizes as
// home, and resolve the live ~/.amqrc.
//
// The private home is also the walk ceiling. Fixtures that re-point HOME at a
// sibling temp dir move the home stop off the walk's ancestor chain; the
// ceiling still stops the walk. Production never sets walkCeilingForTests.
func isolateHome(cleanup *testMainCleanup) string {
	realHome, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(realHome) == "" {
		cleanup.fatalf("resolve real home directory for test isolation: %v", err)
	}
	testRealHome = realHome
	fakeHome, err := os.MkdirTemp(realHome, ".amq-cli-test-home-")
	if err != nil {
		cleanup.fatalf("create isolated test home: %v", err)
	}
	cleanup.add(fakeHome)
	if err := os.Chmod(fakeHome, 0o700); err != nil {
		cleanup.fatalf("secure isolated test home: %v", err)
	}
	testTmpDir := filepath.Join(fakeHome, "tmp")
	if err := os.MkdirAll(testTmpDir, 0o700); err != nil {
		cleanup.fatalf("create isolated test temp dir: %v", err)
	}
	gopath, gomodcache, gocache := realGoCaches(realHome)
	// HOMEDRIVE and HOMEPATH serve Windows tools that resolve the home from
	// the drive-letter pair rather than USERPROFILE.
	setTestEnv(cleanup, map[string]string{
		"HOME":        fakeHome,
		"USERPROFILE": fakeHome,
		"HOMEDRIVE":   filepath.VolumeName(fakeHome),
		"HOMEPATH":    strings.TrimPrefix(fakeHome, filepath.VolumeName(fakeHome)),
		"TMPDIR":      testTmpDir,
		"TMP":         testTmpDir,
		"TEMP":        testTmpDir,
		"GOPATH":      gopath,
		"GOMODCACHE":  gomodcache,
		"GOCACHE":     gocache,
	})
	// Either would move the Claude or Codex home out of the private home.
	setTestEnv(cleanup, map[string]string{"CLAUDE_CONFIG_DIR": "", "CODEX_HOME": ""})

	home, err := os.UserHomeDir()
	if err != nil {
		cleanup.fatalf("resolve test home directory: %v", err)
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		cleanup.fatalf("resolve test home directory symlinks: %v", err)
	}
	walkCeilingForTests = home
	setTestEnv(cleanup, map[string]string{cliTestWalkCeilingEnv: home})
	return home
}

// realGoCaches resolves the toolchain caches from the real home before HOME
// moves. GOPATH and GOCACHE default under $HOME; a module cache inside the
// disposable home re-downloads every run and breaks cleanup, because module
// files are read-only. Values already set in the environment win unchanged.
func realGoCaches(realHome string) (gopath, gomodcache, gocache string) {
	gopath = os.Getenv("GOPATH")
	if gopath == "" {
		gopath = filepath.Join(realHome, "go")
	}
	gomodcache = os.Getenv("GOMODCACHE")
	if gomodcache == "" && gopath != "" {
		gomodcache = filepath.Join(gopath, "pkg", "mod")
	}
	gocache = os.Getenv("GOCACHE")
	if gocache == "" {
		if userCache, err := os.UserCacheDir(); err == nil {
			gocache = filepath.Join(userCache, "go-build")
		}
	}
	return gopath, gomodcache, gocache
}

// enterSecureTempRoot creates the shared secure temp root under the private
// home and makes it the process cwd. The cwd-local routing guard walks up from
// cwd to the Git top looking for an initialized .agent-mail; from the package
// directory of a developer checkout it finds the live queue, and every test
// that pins a temp root fails with a repo-local root conflict (#707). From the
// temp root the walk stops at the private home, which also covers a developer
// whose real HOME is a Git worktree.
func enterSecureTempRoot(cleanup *testMainCleanup, home string) (tempRoot, workingDir string) {
	tempRoot, err := os.MkdirTemp(home, ".amq-cli-test-")
	if err != nil {
		cleanup.fatalf("create secure test temp root: %v", err)
	}
	cleanup.add(tempRoot)
	if err := os.Chmod(tempRoot, 0o700); err != nil {
		cleanup.fatalf("secure test temp root: %v", err)
	}
	cliSecureTempRoot = tempRoot
	workingDir, err = os.Getwd()
	if err != nil {
		cleanup.fatalf("resolve test working directory: %v", err)
	}
	if err := os.Chdir(tempRoot); err != nil {
		cleanup.fatalf("isolate test working directory: %v", err)
	}
	return tempRoot, workingDir
}

// isolateUpdateCache keeps runUpgrade and the update notifier out of the
// developer's real cache (#646). AMQ_CACHE_DIR is the authoritative override,
// because darwin's os.UserCacheDir ignores HOME; XDG_CACHE_HOME covers any path
// that reads it directly.
func isolateUpdateCache(cleanup *testMainCleanup, tempRoot string) {
	testCacheDir := filepath.Join(tempRoot, "cache")
	if err := os.MkdirAll(testCacheDir, 0o700); err != nil {
		cleanup.fatalf("create test cache dir: %v", err)
	}
	setTestEnv(cleanup, map[string]string{
		update.EnvCacheDir: testCacheDir,
		"XDG_CACHE_HOME":   testCacheDir,
	})
}

func setTestEnv(cleanup *testMainCleanup, values map[string]string) {
	for key, value := range values {
		if err := os.Setenv(key, value); err != nil {
			cleanup.fatalf("set %s for tests: %v", key, err)
		}
	}
}
