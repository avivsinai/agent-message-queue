package app

import (
	"fmt"
	"os"
	"testing"
)

// TestMain isolates the wake-readiness cache dir (issue #988, rev-853 P1).
// newWakeReadyPath (internal/keepalive/amq/runner.go) falls back to
// update.DefaultCacheDir(), i.e. os.UserCacheDir, which derives from HOME
// ($HOME/Library/Caches on darwin). This package has no HOME seam, so every
// app-package run created ~/Library/Caches/amq-keepalive/readiness in the
// operator's home. The
// package's own override is AMQ_KEEPALIVE_CACHE_DIR (runner.go), already
// used by the amq package's tests; point it at a disposable dir here so no
// test in this package can write the real cache path. The env override is
// not restored, but the process exits immediately after m.Run.
func TestMain(m *testing.M) {
	cacheDir, err := os.MkdirTemp("", "amq-keepalive-app-test-cache-")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "create isolated keepalive cache dir: %v\n", err)
		os.Exit(1)
	}
	if err := os.Setenv("AMQ_KEEPALIVE_CACHE_DIR", cacheDir); err != nil {
		_ = os.RemoveAll(cacheDir)
		_, _ = fmt.Fprintf(os.Stderr, "set AMQ_KEEPALIVE_CACHE_DIR for tests: %v\n", err)
		os.Exit(1)
	}
	exitCode := m.Run()
	_ = os.RemoveAll(cacheDir)
	os.Exit(exitCode)
}
