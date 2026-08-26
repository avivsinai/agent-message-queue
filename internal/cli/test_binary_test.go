package cli

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// buildTestAMQ signs the test copy after Go links it. macOS 26.5 can reject
// linker-signed ad hoc binaries spawned from a temporary directory with a
// Launch Constraint violation; installed AMQ builds are unaffected.
func buildTestAMQ(t *testing.T, repoRoot, destination string, buildArgs ...string) {
	t.Helper()
	args := make([]string, 0, len(buildArgs)+4)
	args = append(args, "build")
	args = append(args, buildArgs...)
	args = append(args, "-o", destination, "./cmd/amq")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = repoRoot
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("build test amq timed out: %v\n%s", ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("build test amq: %v\n%s", err, output)
	}
	signTestAMQ(t, destination)
}

func copyTestAMQ(t *testing.T, destination string) {
	t.Helper()
	source, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read test binary: %v", err)
	}
	if err := os.WriteFile(destination, data, 0o700); err != nil {
		t.Fatalf("write test amq: %v", err)
	}
	signTestAMQ(t, destination)
}
