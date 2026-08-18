//go:build darwin || linux

package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func buildVersionedWakeRestartBinary(
	t *testing.T,
	repoRoot, destination, version string,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(
		ctx,
		"go", "build",
		"-ldflags", "-X main.version="+version,
		"-o", destination,
		"./cmd/amq",
	)
	cmd.Dir = repoRoot
	cmd.Env = wakeABICleanEnv()
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("build %s timed out: %v\n%s", version, ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("build %s: %v\n%s", version, err, output)
	}
}

func runWakeRestartPTYCommand(t *testing.T, binary string, args ...string) string {
	t.Helper()
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = null.Close() }()
	cmd := exec.Command(binary, args...)
	cmd.Env = os.Environ()
	cmd.Stdin = null
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run %s %v from non-TTY owner child: %v\n%s", binary, args, err, output)
	}
	return string(output)
}

func readWakeRestartPTYDoorbell(t *testing.T, label string) {
	t.Helper()
	type result struct {
		text string
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		var observed strings.Builder
		buffer := make([]byte, 512)
		for observed.Len() < 16*1024 {
			n, err := os.Stdin.Read(buffer)
			if n > 0 {
				_, _ = observed.Write(buffer[:n])
				if strings.Contains(observed.String(), coopWakeDoorbell) &&
					strings.Count(observed.String(), "\n") >= 3 {
					resultCh <- result{text: observed.String()}
					return
				}
			}
			if err != nil {
				resultCh <- result{text: observed.String(), err: err}
				return
			}
		}
		resultCh <- result{text: observed.String(), err: fmt.Errorf("doorbell input exceeded bound")}
	}()
	select {
	case got := <-resultCh:
		if got.err != nil {
			t.Fatalf("read %s doorbell: %v; input=%q", label, got.err, got.text)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out reading %s doorbell from owning PTY", label)
	}
}

func TestWakeRestartRealPTYOwnerHelper(t *testing.T) {
	if os.Getenv(wakeRestartPTYOwnerHelperEnv) == "" {
		t.Skip("external PTY owner helper")
	}
	if !wakeInputIsTTY() {
		t.Fatal("restart owner helper stdin is not the retained PTY")
	}
	root := os.Getenv("AMQ_E2E_ROOT")
	oldBinary := os.Getenv("AMQ_E2E_OLD")
	newBinary := os.Getenv("AMQ_E2E_NEW")
	oldVersion := os.Getenv("AMQ_E2E_OLD_VERSION")
	newVersion := os.Getenv("AMQ_E2E_NEW_VERSION")
	if root == "" || oldBinary == "" || newBinary == "" || oldVersion == "" || newVersion == "" {
		t.Fatal("restart owner helper environment is incomplete")
	}
	oldBinary, err := filepath.EvalSymlinks(oldBinary)
	if err != nil {
		t.Fatal(err)
	}
	newBinary, err = filepath.EvalSymlinks(newBinary)
	if err != nil {
		t.Fatal(err)
	}

	before := inspectWakeLock(root, "codex")
	if !before.Exists || before.Status != wakeLockValid || !before.IdentityConfirmed ||
		before.Lock.ImagePath != oldBinary || before.Lock.ImageVersion != oldVersion ||
		before.Lock.WakeMode != wakeInjectModeRaw {
		t.Fatalf("initial wake = %#v", before)
	}
	prepared, err := validateWakePreparedFileAgainstInspection(root, "codex", before)
	if err != nil || !prepared {
		t.Fatalf("initial prepared proof = %v, err=%v", prepared, err)
	}

	runWakeRestartPTYCommand(
		t,
		newBinary,
		"send", "--root", root, "--me", "user", "--to", "codex",
		"--subject", "pre-restart", "--body", "before",
	)
	readWakeRestartPTYDoorbell(t, "pre-restart")
	// The fixed payload arrives before the final submit/rescue bytes. Let the
	// synchronous raw-delivery state settle before asking for a quiescent exec.
	time.Sleep(500 * time.Millisecond)

	restartOutput := runWakeRestartPTYCommand(
		t,
		newBinary,
		"wake", "restart", "--root", root, "--me", "codex", "--json",
	)
	if !strings.Contains(restartOutput, `"status": "restarted"`) {
		t.Fatalf("restart result = %s", restartOutput)
	}
	after := inspectWakeLock(root, "codex")
	requestedImage, err := captureWakeImageEvidence(newBinary, newVersion)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Exists || after.Status != wakeLockValid || !after.IdentityConfirmed ||
		after.PID != before.PID || after.Lock.Generation == before.Lock.Generation ||
		after.Lock.ImageVersion != newVersion ||
		after.Lock.RunningImageEvidence == nil ||
		after.Lock.ImagePath != after.Lock.RunningImageEvidence.ExecutionPath ||
		!sameRequestedAndBoundWakeImageEvidence(requestedImage, *after.Lock.RunningImageEvidence) {
		t.Fatalf("restarted wake before=%#v after=%#v", before, after)
	}
	prepared, err = validateWakePreparedFileAgainstInspection(root, "codex", after)
	if err != nil || !prepared {
		t.Fatalf("restarted prepared proof = %v, err=%v", prepared, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "agents", "codex", wakeRestartFileName)); !os.IsNotExist(err) {
		t.Fatalf("ready restart record survived: %v", err)
	}
	readWakeRestartPTYDoorbell(t, "pre-restart redelivery")

	firstRestartImagePath := after.Lock.ImagePath
	secondRestartOutput := runWakeRestartPTYCommand(
		t,
		newBinary,
		"wake", "restart", "--root", root, "--me", "codex", "--json",
	)
	if !strings.Contains(secondRestartOutput, `"status": "restarted"`) {
		t.Fatalf("second restart result = %s", secondRestartOutput)
	}
	second := inspectWakeLock(root, "codex")
	if !second.Exists || second.Status != wakeLockValid || !second.IdentityConfirmed ||
		second.PID != after.PID || second.Lock.Generation == after.Lock.Generation ||
		second.Lock.ImageVersion != newVersion || second.Lock.RunningImageEvidence == nil ||
		second.Lock.ImagePath != second.Lock.RunningImageEvidence.ExecutionPath ||
		!sameRequestedAndBoundWakeImageEvidence(requestedImage, *second.Lock.RunningImageEvidence) {
		t.Fatalf("second restarted wake first=%#v second=%#v", after, second)
	}
	prepared, err = validateWakePreparedFileAgainstInspection(root, "codex", second)
	if err != nil || !prepared {
		t.Fatalf("second restarted prepared proof = %v, err=%v", prepared, err)
	}
	if runtime.GOOS == "darwin" {
		if _, err := os.Lstat(firstRestartImagePath); !os.IsNotExist(err) {
			t.Fatalf("first Darwin restart stage survived the second prepared restart: %v", err)
		}
		if stable, err := wakeRestartStageUsesStableStatePlatform(second.Lock.ImagePath); err != nil || !stable {
			t.Fatalf("second Darwin restart stage is not under stable state: path=%s err=%v", second.Lock.ImagePath, err)
		}
		agentStages := filepath.Dir(filepath.Dir(second.Lock.ImagePath))
		stages, readErr := os.ReadDir(agentStages)
		if readErr != nil || len(stages) != 1 || !stages[0].IsDir() ||
			filepath.Join(agentStages, stages[0].Name()) != filepath.Dir(second.Lock.ImagePath) {
			t.Fatalf("Darwin restart stages after second restart = %v, err=%v", stages, readErr)
		}
	}
	after = second

	runWakeRestartPTYCommand(
		t,
		newBinary,
		"send", "--root", root, "--me", "user", "--to", "codex",
		"--subject", "post-restart", "--body", "after",
	)
	readWakeRestartPTYDoorbell(t, "post-restart")
	drained := runWakeRestartPTYCommand(
		t,
		newBinary,
		"drain", "--root", root, "--me", "codex", "--include-body",
	)
	if !strings.Contains(drained, "Subject: pre-restart") ||
		!strings.Contains(drained, "Subject: post-restart") {
		t.Fatalf("restart drain did not retain both cohorts:\n%s", drained)
	}
	fmt.Printf(
		"AMQ_RESTART_HELPER_OK pid=%d old_generation=%s new_generation=%s image=%s prepared=true restarts=2\n",
		after.PID,
		before.Lock.Generation,
		after.Lock.Generation,
		after.Lock.ImagePath,
	)
}
