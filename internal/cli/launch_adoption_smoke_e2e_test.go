//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func resolveRealAMQBinary(t *testing.T) string {
	t.Helper()
	if path := strings.TrimSpace(os.Getenv("AMQ_LAUNCH_LIVE_BINARY")); path != "" {
		if !filepath.IsAbs(path) {
			t.Fatalf("AMQ_LAUNCH_LIVE_BINARY must be an absolute path, got %q", path)
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatalf("AMQ_LAUNCH_LIVE_BINARY %q: %v", path, err)
		}
		info, err := os.Stat(resolved)
		if err != nil || info.IsDir() {
			t.Fatalf("AMQ_LAUNCH_LIVE_BINARY %q is not a file", path)
		}
		return resolved
	}
	return buildAdoptionSmokeAMQ(t)
}

func buildAdoptionSmokeAMQ(t *testing.T) string {
	t.Helper()
	repoRoot, err := cliTestRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	amqBinary := filepath.Join(t.TempDir(), "amq")
	buildTestAMQ(t, repoRoot, amqBinary)
	return amqBinary
}
