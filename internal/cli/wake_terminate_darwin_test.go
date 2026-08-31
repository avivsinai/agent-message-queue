//go:build darwin

package cli

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
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

func TestDarwinRawTerminationRefusesPostAttemptGuardFailure(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	const agent = "codex"
	establishDoctorWakeLifecycleGuardForTest(t, root, agent)
	lockPath := writeWakeLockForTest(t, root, agent, wakeLock{
		PID:          wakePID,
		TTY:          "unknown",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
		Args:         []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", agent},
		WakeMode:     wakeInjectModeRaw,
		Generation:   "raw-guard-race",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:                      pid,
			Running:                  true,
			StartToken:               "start-1",
			BootID:                   "boot-1",
			Executable:               "/opt/homebrew/bin/amq",
			Args:                     []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", agent},
			ControllingTerminalKnown: true,
			HasControllingTerminal:   false,
		}
	})

	startHolder := make(chan struct{})
	releaseHolder := make(chan struct{})
	holderEntered := make(chan struct{})
	holderDone := make(chan error, 1)
	var startOnce, releaseOnce sync.Once
	start := func() { startOnce.Do(func() { close(startHolder) }) }
	release := func() { releaseOnce.Do(func() { close(releaseHolder) }) }
	go func() {
		select {
		case <-startHolder:
		case <-releaseHolder:
			holderDone <- nil
			return
		}
		holderDone <- withWakeLifecycleGuard(root, agent, func() error {
			close(holderEntered)
			<-releaseHolder
			return nil
		})
	}()
	t.Cleanup(func() {
		release()
		start()
		if err := <-holderDone; err != nil {
			t.Errorf("release lifecycle guard holder: %v", err)
		}
	})

	oldSeam := afterDarwinWakeSignalValidation
	afterDarwinWakeSignalValidation = func() {
		start()
		select {
		case <-holderEntered:
		case <-time.After(time.Second):
			t.Fatal("lifecycle guard holder did not enter")
		}
	}
	t.Cleanup(func() { afterDarwinWakeSignalValidation = oldSeam })

	agentDir, err := openExistingCoopWakeAgentDir(root, agent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()

	inspection := inspectWakeLock(root, agent)
	retired, err := terminateAndRemoveOrphanedWakeLockInDirWithRawConsent(
		agentDir,
		inspection,
		true,
		nil,
	)
	if retired {
		t.Fatal("raw operator-only refusal was reported as retirement")
	}
	if err == nil || !strings.Contains(err.Error(), "held by another process") {
		t.Fatalf("raw termination error = %v, want post-attempt guard refusal", err)
	}
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("raw lock changed after guard refusal: %v", statErr)
	}
}

func TestDarwinRawTerminationDoesNotClaimRetiredWhenLockChanges(t *testing.T) {
	const wakePID = 4243
	root := secureTempDirForTest(t)
	const agent = "codex"
	establishDoctorWakeLifecycleGuardForTest(t, root, agent)
	lockPath := writeWakeLockForTest(t, root, agent, wakeLock{
		PID:          wakePID,
		TTY:          "unknown",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/homebrew/bin/amq",
		Args:         []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", agent},
		WakeMode:     wakeInjectModeRaw,
		Generation:   "raw-lock-change",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:                      pid,
			Running:                  true,
			StartToken:               "start-1",
			BootID:                   "boot-1",
			Executable:               "/opt/homebrew/bin/amq",
			Args:                     []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", agent},
			ControllingTerminalKnown: true,
			HasControllingTerminal:   false,
		}
	})
	oldSeam := afterDarwinWakeSignalValidation
	afterDarwinWakeSignalValidation = func() {
		if err := os.Remove(lockPath); err != nil {
			t.Fatalf("remove lock after operator-only refusal: %v", err)
		}
	}
	t.Cleanup(func() { afterDarwinWakeSignalValidation = oldSeam })

	agentDir, err := openExistingCoopWakeAgentDir(root, agent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()

	inspection := inspectWakeLock(root, agent)
	retired, err := terminateAndRemoveOrphanedWakeLockInDirWithRawConsent(
		agentDir,
		inspection,
		true,
		nil,
	)
	if retired {
		t.Fatal("operator-only raw refusal was reported as retirement after lock change")
	}
	var operatorOnly *wakeOperatorOnlyError
	if !errors.As(err, &operatorOnly) || operatorOnly.RestartCapability() != wakeRestartOperatorOnly {
		t.Fatalf("raw termination error = %T %v, want typed operator_only refusal", err, err)
	}
	if _, statErr := os.Stat(lockPath); !os.IsNotExist(statErr) {
		t.Fatalf("lock still exists after test interleaving, stat err=%v", statErr)
	}
}
