//go:build linux

package cli

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestLinuxWakeSelfUpgradeRealPTYStableSymlink(t *testing.T) {
	if testing.Short() {
		t.Skip("real Linux PTY self-upgrade E2E")
	}
	legacyTIOCSTI, err := os.ReadFile("/proc/sys/dev/tty/legacy_tiocsti")
	if err != nil {
		t.Skipf("raw PTY self-upgrade E2E requires readable legacy_tiocsti opt-in: %v", err)
	}
	if strings.TrimSpace(string(legacyTIOCSTI)) != "1" {
		t.Skip("raw PTY self-upgrade E2E requires /proc/sys/dev/tty/legacy_tiocsti=1")
	}

	stable, oldBinary, newBinary, root, oldVersion, newVersion := prepareWakeSelfUpgradeE2E(t)
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ownerCommand := []string{
		stable,
		"coop", "exec",
		"--root", root,
		"--me", "codex",
		"--require-wake",
		"--wake-inject-mode", "raw",
		testBinary,
		"-test.run=^TestWakeSelfUpgradeRealPTYOwnerHelper$",
	}
	quoted := make([]string, 0, len(ownerCommand))
	for _, arg := range ownerCommand {
		quoted = append(quoted, wakeABIShellQuoteArg(arg))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/script", "-q", "-e", "-c", strings.Join(quoted, " "), "/dev/null")
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
		t.Fatalf("Linux self-upgrade PTY E2E timed out: %v\n%s", ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("Linux self-upgrade PTY E2E: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "AMQ_SELF_UPGRADE_HELPER_OK") {
		t.Fatalf("Linux self-upgrade helper proof missing:\n%s", output)
	}
}
