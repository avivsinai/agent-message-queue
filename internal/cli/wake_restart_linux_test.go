//go:build linux

package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

const wakeRestartBoundExecHelperEnv = "AMQ_TEST_WAKE_RESTART_BOUND_EXEC"

func TestLinuxWakeRestartBoundPayload(t *testing.T) {
	if os.Getenv(wakeRestartBoundExecHelperEnv) != "payload" {
		t.Skip("bound-image payload helper")
	}
	_, _ = os.Stdout.WriteString("BOUND_IMAGE_A\n")
}

func TestLinuxWakeRestartBoundExecHelper(t *testing.T) {
	if os.Getenv(wakeRestartBoundExecHelperEnv) != "exec" {
		t.Skip("bound-image exec helper")
	}
	env := setEnvVar(os.Environ(), wakeRestartBoundExecHelperEnv, "payload")
	err := syscall.Exec(
		"/proc/self/fd/3",
		[]string{"bound-amq", "-test.run=^TestLinuxWakeRestartBoundPayload$"},
		env,
	)
	t.Fatalf("exec bound image: %v", err)
}

func TestLinuxWakeRestartBindingSurvivesPublicPathSwap(t *testing.T) {
	dir := t.TempDir()
	publicPath := filepath.Join(dir, "amq")
	copyTestAMQ(t, publicPath)
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := captureWakeImageEvidence(publicPath, "bound-swap-test")
	if err != nil {
		t.Fatal(err)
	}
	bound, err := bindWakeRestartCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := bound.close(); err != nil {
			t.Error(err)
		}
	}()

	preflight := exec.Command("/proc/self/fd/3", "-test.run=^TestLinuxWakeRestartBoundPayload$")
	preflight.ExtraFiles = []*os.File{bound.file}
	preflight.Env = setEnvVar(os.Environ(), wakeRestartBoundExecHelperEnv, "payload")
	if output, err := preflight.CombinedOutput(); err != nil || !strings.Contains(string(output), "BOUND_IMAGE_A") {
		t.Fatalf("execute bound preflight image: err=%v output=%q", err, output)
	}

	binaryB, err := os.ReadFile("/usr/bin/false")
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(dir, "amq.replacement")
	if err := os.WriteFile(replacement, binaryB, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, publicPath); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(publicPath).Run(); err == nil {
		t.Fatal("normal public path still executed image A after atomic replacement")
	}

	helper := exec.Command(testBinary, "-test.run=^TestLinuxWakeRestartBoundExecHelper$")
	helper.ExtraFiles = []*os.File{bound.file}
	helper.Env = setEnvVar(os.Environ(), wakeRestartBoundExecHelperEnv, "exec")
	output, err := helper.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "BOUND_IMAGE_A") {
		t.Fatalf("execute parent-FD-bound image after swap: err=%v output=%q", err, output)
	}
}

func TestLinuxWakeRestartRealPTYPreservesPIDAndUnreadWork(t *testing.T) {
	if testing.Short() {
		t.Skip("real Linux PTY restart E2E")
	}
	legacyTIOCSTI, err := os.ReadFile("/proc/sys/dev/tty/legacy_tiocsti")
	if err != nil {
		t.Skipf("raw PTY restart E2E requires readable legacy_tiocsti opt-in: %v", err)
	}
	if strings.TrimSpace(string(legacyTIOCSTI)) != "1" {
		t.Skip("raw PTY restart E2E requires /proc/sys/dev/tty/legacy_tiocsti=1")
	}
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve restart test source path")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(testFile), "..", ".."))
	temp := t.TempDir()
	oldDir := filepath.Join(temp, "old")
	newDir := filepath.Join(temp, "new")
	if err := os.MkdirAll(oldDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(newDir, 0o700); err != nil {
		t.Fatal(err)
	}
	oldBinary := filepath.Join(oldDir, "amq")
	newBinary := filepath.Join(newDir, "amq")
	const oldVersion = "0.56.0-e2e-old"
	const newVersion = "0.56.0-e2e-new"
	buildVersionedWakeRestartBinary(t, repoRoot, oldBinary, oldVersion)
	buildVersionedWakeRestartBinary(t, repoRoot, newBinary, newVersion)

	root := filepath.Join(temp, "root")
	initCmd := exec.Command(oldBinary, "init", "--root", root, "--agents", "codex")
	initCmd.Env = wakeABICleanEnv()
	if output, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("init Linux restart E2E: %v\n%s", err, output)
	}
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ownerCommand := []string{
		oldBinary,
		"coop", "exec",
		"--root", root,
		"--me", "codex",
		"--require-wake",
		"--wake-inject-mode", "raw",
		testBinary,
		"-test.run=^TestWakeRestartRealPTYOwnerHelper$",
	}
	quoted := make([]string, 0, len(ownerCommand))
	for _, arg := range ownerCommand {
		quoted = append(quoted, wakeABIShellQuoteArg(arg))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(
		ctx,
		"/usr/bin/script",
		"-q", "-e", "-c", strings.Join(quoted, " "),
		"/dev/null",
	)
	cmd.Env = wakeABICleanEnv(
		wakeRestartPTYOwnerHelperEnv+"=1",
		"AMQ_E2E_ROOT="+root,
		"AMQ_E2E_OLD="+oldBinary,
		"AMQ_E2E_NEW="+newBinary,
		"AMQ_E2E_OLD_VERSION="+oldVersion,
		"AMQ_E2E_NEW_VERSION="+newVersion,
	)
	ptyInput, keepPTYInputOpen, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ptyInput.Close() }()
	defer func() { _ = keepPTYInputOpen.Close() }()
	cmd.Stdin = ptyInput
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("Linux restart PTY E2E timed out: %v\n%s", ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("Linux restart PTY E2E: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "AMQ_RESTART_HELPER_OK") {
		t.Fatalf("Linux restart PTY helper proof missing:\n%s", output)
	}

	agentPath := filepath.Join(root, "agents", "codex")
	deadline := time.Now().Add(5 * time.Second)
	for {
		remaining := make([]string, 0, 3)
		for _, name := range []string{".wake.lock", ".wake.prepared", ".wake.restart"} {
			if _, statErr := os.Lstat(filepath.Join(agentPath, name)); statErr == nil {
				remaining = append(remaining, name)
			} else if !os.IsNotExist(statErr) {
				t.Fatal(statErr)
			}
		}
		if len(remaining) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Linux wake lifecycle files survived owner exit: %v", remaining)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
