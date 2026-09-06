//go:build linux

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const wakeRestartBoundExecHelperEnv = "AMQ_TEST_WAKE_RESTART_BOUND_EXEC"

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
