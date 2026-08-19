//go:build darwin || linux

package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestRetireWakeIfGenerationRefusesReplacementAndPreservesG2(t *testing.T) {
	const (
		oldPID         = 4242
		replacementPID = 4343
		observed       = "0123456789abcdef0123456789abcdef"
		replacement    = "fedcba9876543210fedcba9876543210"
	)
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	requested, lockPath := installRetireWakeFixture(t, root, "codex", injector, []string{"exec", "terminal-a"}, oldPID)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return matchingRetireWakeProcess(pid, root, "codex", injector)
	})
	stubWakeCheckRuntime(t, false, "0.49.14")

	output, err := captureEnvStdout(t, func() error {
		return runWake([]string{"check", "--root", root, "--me", "codex", "--json"})
	})
	if err != nil {
		t.Fatalf("wake check: %v", err)
	}
	var observedCheck struct {
		WakeGeneration   string `json:"wake_generation"`
		WakeTargetDigest string `json:"wake_target_digest"`
	}
	if err := json.Unmarshal([]byte(output), &observedCheck); err != nil {
		t.Fatalf("decode wake check: %v\n%s", err, output)
	}
	if observedCheck.WakeGeneration != observed {
		t.Fatalf("observed generation = %q, want %q", observedCheck.WakeGeneration, observed)
	}
	if observedCheck.WakeTargetDigest == "" {
		t.Fatal("observed check omitted wake_target_digest")
	}

	replacementTarget := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec", "terminal-b"})
	if err := writeWakeTarget(root, "codex", replacementTarget); err != nil {
		t.Fatalf("write replacement target: %v", err)
	}
	replacementLock := bindWakeLockToTarget(wakeLock{
		PID:          replacementPID,
		TTY:          "unknown",
		ProcessStart: "wake-start",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
		Args:         []string{"amq", "wake", "--root", root, "--me", "codex", "--inject-via", injector},
		Generation:   replacement,
	}, replacementTarget)
	replacementLock.ControlSocket = wakeControlSocketPath(root, "codex", replacementLock.Generation)
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	writeWakeLockForTest(t, root, "codex", replacementLock)

	result, retireErr := retireWakeIfGeneration(root, "codex", requested, observed)
	if retireErr == nil || result.Status != "refused" || !strings.Contains(result.Reason, "generation changed") {
		t.Fatalf("retire result = %#v err=%v, want generation CAS refusal", result, retireErr)
	}

	current := inspectWakeLock(root, "codex")
	if !current.Exists || current.PID != replacementPID || current.Lock.Generation != replacement {
		t.Fatalf("G2 did not survive: %#v", current)
	}
	persisted, exists, err := readWakeTarget(root, "codex")
	if err != nil || !exists || !sameWakeInjectorIdentity(persisted, replacementTarget) {
		t.Fatalf("G2 target changed: (%#v,%v,%v)", persisted, exists, err)
	}
}
