//go:build darwin || linux

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/keepalive/registry"
)

func TestKeepaliveSelfUpgradeLive(t *testing.T) {
	if os.Getenv("AMQ_KEEPALIVE_LIVE") != "1" {
		t.Skip("set AMQ_KEEPALIVE_LIVE=1 for the real keepalive self-upgrade proof")
	}
	if testing.Short() {
		t.Skip("real keepalive self-upgrade proof")
	}

	dir := t.TempDir()
	repoRoot := keepaliveAppRepoRoot(t)
	releases := filepath.Join(dir, "releases")
	install := filepath.Join(dir, "install")
	if err := os.MkdirAll(releases, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(install, 0o700); err != nil {
		t.Fatal(err)
	}
	oldBinary := filepath.Join(releases, "old", "amq-keepalive")
	newBinary := filepath.Join(releases, "new", "amq-keepalive")
	if err := os.MkdirAll(filepath.Dir(oldBinary), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(newBinary), 0o700); err != nil {
		t.Fatal(err)
	}
	buildKeepaliveVersionedBinary(t, repoRoot, oldBinary, "1.0.0")
	buildKeepaliveVersionedBinary(t, repoRoot, newBinary, "1.1.0")
	stable := filepath.Join(install, "amq-keepalive")
	if err := os.Rename(oldBinary, stable); err != nil {
		t.Fatalf("install old keepalive: %v", err)
	}

	registryPath := filepath.Join(install, "registry.json")
	target := filepath.Join(dir, "supervised-target")
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.New(registryPath).Upsert(registry.Entry{
		Root: "/tmp/amq-keepalive-live", Agent: "codex", Adapter: "file", Target: target,
	}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	fakeAMQ := filepath.Join(dir, "amq")
	callLog := filepath.Join(dir, "amq-calls.log")
	writeKeepaliveLiveFakeAMQ(t, fakeAMQ)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, stable, "supervise", "--registry", registryPath, "--amq", fakeAMQ, "--self", stable, "--interval", "20ms")
	cmd.Env = append(os.Environ(), "AMQ_KEEPALIVE_LIVE=1", "AMQ_KEEPALIVE_LIVE_CALLS="+callLog)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_, _ = cmd.Process.Wait()
	})

	statePath := filepath.Join(install, selfUpgradeStateFileName)
	waitForKeepaliveVersion(t, statePath, "1.0.0")
	registryBefore, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatalf("read initial registry: %v", err)
	}
	callsBefore, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read initial AMQ calls: %v", err)
	}

	next := stable + ".next"
	if err := os.Rename(newBinary, next); err != nil {
		t.Fatalf("stage newer keepalive: %v", err)
	}
	if err := os.Rename(next, stable); err != nil {
		t.Fatalf("publish newer keepalive: %v", err)
	}
	waitForKeepaliveVersion(t, statePath, "1.1.0")
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("keepalive pid %d is not alive after exec: %v\n%s", pid, err, output.String())
	}
	registryAfter, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatalf("read upgraded registry: %v", err)
	}
	if !bytes.Equal(registryBefore, registryAfter) {
		t.Fatalf("registry changed across self-upgrade:\nbefore=%safter=%s", registryBefore, registryAfter)
	}
	callsAfter, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read upgraded AMQ calls: %v", err)
	}
	if !bytes.Equal(callsBefore, callsAfter) {
		t.Fatalf("supervised wake was touched during self-upgrade:\nbefore=%qafter=%q", callsBefore, callsAfter)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("stop keepalive: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("keepalive exit: %v\n%s", err, output.String())
	}
}

func buildKeepaliveVersionedBinary(t *testing.T, repoRoot, output, version string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-ldflags", "-X main.version="+version, "-o", output, "./cmd/amq-keepalive")
	cmd.Dir = repoRoot
	if outputBytes, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build keepalive %s: %v\n%s", version, err, outputBytes)
	}
}

func writeKeepaliveLiveFakeAMQ(t *testing.T, path string) {
	t.Helper()
	const script = `#!/bin/sh
printf '%s\n' "$*" >> "$AMQ_KEEPALIVE_LIVE_CALLS"
if [ "$1" = "wake" ] && [ "$2" = "check" ]; then
  printf '%s\n' '{"schema":1,"live_wake":false,"image_status":"none"}'
  exit 0
fi
if [ "$1" = "wake" ]; then
  ready=""
  previous=""
  for arg in "$@"; do
    if [ "$previous" = "-ready-file" ]; then ready="$arg"; fi
    previous="$arg"
  done
  [ -n "$ready" ] || exit 11
  umask 077
  printf '%s\n' '{"schema":1,"generation":"live-generation","target_digest":"live-digest"}' > "$ready"
  exit 0
fi
exit 12
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

func waitForKeepaliveVersion(t *testing.T, statePath, want string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(statePath)
		if err == nil {
			var state selfUpgradeStateFile
			if json.Unmarshal(data, &state) == nil && state.IncumbentVersion == want {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	data, _ := os.ReadFile(statePath)
	t.Fatalf("keepalive did not publish incumbent version %q at %s: %s", want, statePath, fmt.Sprint(data))
}

func keepaliveAppRepoRoot(t *testing.T) string {
	t.Helper()
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve app test source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(testFile), "..", "..", ".."))
}
