//go:build darwin || linux

package cli

import (
	"testing"
)

func buildVersionedWakeRestartBinary(
	t *testing.T,
	repoRoot, destination, version string,
) {
	t.Helper()
	buildTestAMQ(t, repoRoot, destination, "-ldflags", "-X main.version="+version)
}
