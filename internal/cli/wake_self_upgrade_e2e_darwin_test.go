//go:build darwin

package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDarwinWakeSelfUpgradeRealPTYStableSymlink(t *testing.T) {
	if testing.Short() {
		t.Skip("real Darwin PTY self-upgrade E2E")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stable, oldBinary, newBinary, root, oldVersion, newVersion := prepareWakeSelfUpgradeE2E(t)
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ptyLog := filepath.Join(t.TempDir(), "pty.log")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(
		ctx,
		"/usr/bin/script", "-q", ptyLog,
		stable,
		"coop", "exec",
		"--root", root,
		"--me", "codex",
		"--require-wake",
		"--wake-inject-mode", "raw",
		testBinary,
		"-test.run=^TestWakeSelfUpgradeRealPTYOwnerHelper$",
	)
	cmd.Env = wakeSelfUpgradeE2EEnv(root, stable, oldBinary, newBinary, oldVersion, newVersion)
	ptyInput, keepPTYInputOpen, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ptyInput.Close() }()
	defer func() { _ = keepPTYInputOpen.Close() }()
	cmd.Stdin = ptyInput
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("Darwin self-upgrade PTY E2E timed out: %v\n%s", ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("Darwin self-upgrade PTY E2E: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "AMQ_SELF_UPGRADE_HELPER_OK") {
		t.Fatalf("Darwin self-upgrade helper proof missing:\n%s", output)
	}
}
