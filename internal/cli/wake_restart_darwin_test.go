//go:build darwin

package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDarwinWakeRestartBoundPayload(t *testing.T) {
	if os.Getenv("AMQ_TEST_WAKE_RESTART_BOUND_EXEC") != "payload" {
		t.Skip("bound-image payload helper")
	}
	_, _ = os.Stdout.WriteString("BOUND_IMAGE_A\n")
}

func TestDarwinStagedWakeImageCTimeExceptionDoesNotWeakenIdentityOrContent(t *testing.T) {
	base, err := captureCurrentWakeImageEvidence()
	if err != nil {
		t.Fatal(err)
	}
	base.Method = wakeImageMethodPathnameExecVerified
	base.ExecutionPath = filepath.Join(t.TempDir(), ".amq.amq-restart-test", "amq")
	ctimeOnly := base
	ctimeOnly.CTimeNS++
	if !sameDarwinStagedWakeImageEvidence(base, ctimeOnly) {
		t.Fatal("staged Darwin ctime-only namespace mutation was rejected")
	}
	mutations := []struct {
		name   string
		mutate func(*wakeImageEvidenceV1)
	}{
		{name: "device", mutate: func(value *wakeImageEvidenceV1) { value.Device++ }},
		{name: "inode", mutate: func(value *wakeImageEvidenceV1) { value.Inode++ }},
		{name: "size", mutate: func(value *wakeImageEvidenceV1) { value.Size++ }},
		{name: "content", mutate: func(value *wakeImageEvidenceV1) { value.SHA256 = "sha256:" + strings.Repeat("0", 64) }},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			changed := ctimeOnly
			test.mutate(&changed)
			if sameDarwinStagedWakeImageEvidence(base, changed) {
				t.Fatalf("staged Darwin %s mutation was accepted", test.name)
			}
		})
	}
}

func TestDarwinWakeRestartBindingSurvivesPublicPathSwapAndCleansStage(t *testing.T) {
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binaryA, err := os.ReadFile(testBinary)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	publicPath := filepath.Join(dir, "amq")
	if err := os.WriteFile(publicPath, binaryA, 0o700); err != nil {
		t.Fatal(err)
	}
	candidate, err := captureWakeImageEvidence(publicPath, "bound-swap-test")
	if err != nil {
		t.Fatal(err)
	}
	// Link creation is the one intentional ctime mutation in this protocol.
	// Cross a timestamp boundary so the regression proves that the narrow
	// Darwin exception is exercised rather than accidentally unused.
	time.Sleep(1100 * time.Millisecond)
	bound, err := bindWakeRestartCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if bound.evidence.CTimeNS == candidate.CTimeNS {
		_ = bound.close()
		t.Fatal("Darwin hardlink did not change the shared inode ctime")
	}
	if !sameRequestedAndBoundWakeImageEvidence(candidate, bound.evidence) {
		_ = bound.close()
		t.Fatal("Darwin hardlink ctime exception did not preserve exact stable evidence")
	}
	stagePath := bound.executionPath
	stageDir := bound.stageDir

	binaryB, err := os.ReadFile("/usr/bin/false")
	if err != nil {
		_ = bound.close()
		t.Fatal(err)
	}
	replacement := filepath.Join(dir, "amq.replacement")
	if err := os.WriteFile(replacement, binaryB, 0o700); err != nil {
		_ = bound.close()
		t.Fatal(err)
	}
	if err := os.Rename(replacement, publicPath); err != nil {
		_ = bound.close()
		t.Fatal(err)
	}
	if err := exec.Command(publicPath).Run(); err == nil {
		_ = bound.close()
		t.Fatal("normal public path still executed image A after atomic replacement")
	}
	command := exec.Command(stagePath, "-test.run=^TestDarwinWakeRestartBoundPayload$")
	command.Env = setEnvVar(os.Environ(), "AMQ_TEST_WAKE_RESTART_BOUND_EXEC", "payload")
	output, err := command.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "BOUND_IMAGE_A") {
		_ = bound.close()
		t.Fatalf("execute staged bound image after swap: err=%v output=%q", err, output)
	}
	if err := bound.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stagePath); !os.IsNotExist(err) {
		t.Fatalf("staged image survived exact cleanup: %v", err)
	}
	if _, err := os.Lstat(stageDir); !os.IsNotExist(err) {
		t.Fatalf("staged directory survived exact cleanup: %v", err)
	}
}

func TestDarwinWakeRestartRealPTYPreservesPIDAndUnreadWork(t *testing.T) {
	if testing.Short() {
		t.Skip("real PTY restart E2E")
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
		t.Fatalf("init restart E2E: %v\n%s", err, output)
	}
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ptyLog := filepath.Join(temp, "pty.log")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(
		ctx,
		"/usr/bin/script", "-q", ptyLog,
		oldBinary,
		"coop", "exec",
		"--root", root,
		"--me", "codex",
		"--require-wake",
		"--wake-inject-mode", "raw",
		testBinary,
		"-test.run=^TestWakeRestartRealPTYOwnerHelper$",
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
		t.Fatalf("restart PTY E2E timed out: %v\n%s", ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("restart PTY E2E: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "AMQ_RESTART_HELPER_OK") {
		t.Fatalf("restart PTY helper proof missing:\n%s", output)
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
			t.Fatalf("wake lifecycle files survived owner exit: %v", remaining)
		}
		time.Sleep(25 * time.Millisecond)
	}
	stages, err := filepath.Glob(filepath.Join(newDir, ".amq.amq-restart-*"))
	if err != nil || len(stages) != 0 {
		t.Fatalf("Darwin restart stages survived owner exit: %v, err=%v", stages, err)
	}
}
