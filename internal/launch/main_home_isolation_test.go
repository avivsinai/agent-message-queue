package launch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain isolates HOME for the whole package run (issue #988). The tmux
// backend tests spawn real `tmux new-session` shells; a spawned login shell
// inherits the test process environment and writes its history file into
// $HOME, so without the seam a suite run on a machine with a live home
// appends to the operator's real ~/.zsh_history. The seam points HOME,
// USERPROFILE and the Windows drive-letter pair (HOMEDRIVE/HOMEPATH) at one
// fresh temp dir, pins the Go toolchain caches to their pre-override
// locations (commands_test builds the amq binary), and restores everything
// after m.Run(). Per-test t.Setenv("HOME", ...) keeps working.
func TestMain(m *testing.M) {
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

	fakeHome, err := os.MkdirTemp("", "amq-launch-test-home-")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "create isolated test home: %v\n", err)
		os.Exit(1)
	}
	if err := os.Chmod(fakeHome, 0o700); err != nil {
		_ = os.RemoveAll(fakeHome)
		_, _ = fmt.Fprintf(os.Stderr, "secure isolated test home: %v\n", err)
		os.Exit(1)
	}
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

	exitCode := m.Run()
	if err := os.RemoveAll(fakeHome); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "remove isolated test home: %v\n", err)
		exitCode = 1
	}
	os.Exit(exitCode)
}

// TestTestHomeIsIsolatedFromRealHome pins the seam: during the package run
// os.UserHomeDir must resolve inside the temp tree, never the operator's
// real home (issue #988). Reverting the TestMain override fails this
// everywhere, not only on a machine with a live ~/.zsh_history.
func TestTestHomeIsIsolatedFromRealHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(home, os.TempDir()) {
		t.Fatalf("test home %s is not inside the temp tree; the real home leaked into the package run", home)
	}
}
