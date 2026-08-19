//go:build darwin

package cli

import (
	"errors"
	"os"
	"testing"
)

func TestTerminateWakeProcessDoesNotSignalReplacementBetweenValidationAndKill(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          wakePID,
		TTY:          "/dev/amq-missing-tty",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
		Args:         []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", "codex"},
		WakeMode:     wakeInjectModeRaw,
		Generation:   "observed-generation",
	})
	replaced := false
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid != wakePID {
			return wakeProcessInfo{PID: pid}
		}
		info := wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: "start-1",
			BootID:     "boot-1",
			Executable: "/opt/homebrew/bin/amq",
			Args:       []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", "codex"},
		}
		if replaced {
			info.StartToken = "replacement-start"
			info.Executable = "/bin/sleep"
			info.Args = []string{"/bin/sleep", "100"}
		}
		return info
	})
	var signals []os.Signal
	stubSignalWakeProcess(t, func(pid int, sig os.Signal) error {
		signals = append(signals, sig)
		t.Errorf("signaled replacement pid=%d sig=%v", pid, sig)
		return nil
	})

	oldSeam := afterDarwinWakeSignalValidation
	afterDarwinWakeSignalValidation = func() {
		replaced = true
		writeWakeLockForTest(t, root, "codex", wakeLock{
			PID:          wakePID,
			TTY:          "/dev/amq-missing-tty",
			ProcessStart: "replacement-start",
			BootID:       "boot-1",
			Executable:   "/bin/sleep",
			Args:         []string{"/bin/sleep", "100"},
			WakeMode:     wakeInjectModeRaw,
			Generation:   "replacement-generation",
		})
	}
	t.Cleanup(func() { afterDarwinWakeSignalValidation = oldSeam })

	inspection := inspectWakeLock(root, "codex")
	err := terminateWakeProcess(inspection)
	var operatorOnly *wakeOperatorOnlyError
	if !errors.As(err, &operatorOnly) || operatorOnly.RestartCapability() != wakeRestartOperatorOnly {
		t.Fatalf("terminate error = %T %v, want typed operator_only", err, err)
	}
	if len(signals) != 0 {
		t.Fatalf("signals = %v, want none to the replacement", signals)
	}
	current := inspectWakeLock(root, "codex")
	if !current.Exists || current.Lock.Generation != "replacement-generation" {
		t.Fatalf("replacement was not preserved: %#v", current)
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("replacement lock missing: %v", statErr)
	}
}
