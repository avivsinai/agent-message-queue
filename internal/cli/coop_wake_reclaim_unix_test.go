//go:build darwin || linux

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCoopWakeStartupConflictGivesExactStaleSessionRepair(t *testing.T) {
	root := `/tmp/AMQ & stale's $session`
	inspection := wakeLockInspection{
		Exists:   true,
		Status:   wakeLockStale,
		Reason:   "process is not running",
		Root:     root,
		Agent:    "codex",
		LockPath: root + "/agents/codex/.wake.lock",
		Lock: wakeLock{
			PID:     66121,
			TTY:     "/dev/ttys042",
			Started: "2026-07-30T17:00:00Z",
		},
	}
	err := coopWakeStartupConflictError(
		inspection,
		errors.New("amq wake exited before becoming ready"),
	)
	if err == nil {
		t.Fatal("stale startup conflict returned nil")
	}
	want := doctorRootCommandForOS(root, "", runtime.GOOS, "--ops", "--fix-wake-locks")
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("stale conflict = %v, want exact repair %q", err, want)
	}
	if strings.Contains(err.Error(), "use that terminal") {
		t.Fatalf("stale conflict rendered live-owner advice: %v", err)
	}
}

func TestCoopWakeStartupConflictPreservesUnverifiedState(t *testing.T) {
	root := secureTempDirForTest(t)
	inspection := wakeLockInspection{
		Exists:   true,
		Status:   wakeLockUnverified,
		Reason:   "owner identity unavailable",
		Root:     root,
		Agent:    "codex",
		LockPath: root + "/agents/codex/.wake.lock",
		Lock:     wakeLock{PID: 66121},
	}
	err := coopWakeStartupConflictError(
		inspection,
		errors.New("amq wake exited before becoming ready"),
	)
	if err == nil {
		t.Fatal("unverified startup conflict returned nil")
	}
	inspect := doctorRootCommandForOS(root, "", runtime.GOOS, "--ops")
	if !strings.Contains(err.Error(), inspect) ||
		strings.Contains(err.Error(), "--fix-wake-locks") ||
		!strings.Contains(err.Error(), "preserved") {
		t.Fatalf("unverified conflict is not an inspect-only refusal: %v", err)
	}
}

func TestCoopWakeStartupConflictNeverReturnsNil(t *testing.T) {
	if err := coopWakeStartupConflictError(wakeLockInspection{}, nil); err == nil {
		t.Fatal("unclassified startup conflict returned nil")
	}
}

func TestPrepareCoopWakeLockRemovesProvenStaleWithoutPrompt(t *testing.T) {
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{PID: 66121, Generation: "stale"})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo { return wakeProcessInfo{PID: pid} })

	if err := prepareCoopWakeLock(root, "codex", false, "unused"); err != nil {
		t.Fatalf("prepare stale wake lock: %v", err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("stale lock remains: %v", err)
	}
}

func TestPrepareCoopWakeLockDetachedStaleCleansOldLockOnly(t *testing.T) {
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{PID: 66121, Generation: "stale"})
	agentPath := filepath.Dir(lockPath)
	var detachedPath string
	var successorBefore map[string]wakeCheckTreeEntry
	detached := false
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if !detached {
			detached = true
			detachedPath = detachGenericWakeAgentDirForTest(t, agentPath, ".wake.lock")
			successorBefore = snapshotWakeCheckTree(t, agentPath)
		}
		return wakeProcessInfo{PID: pid}
	})

	err := prepareCoopWakeLock(root, "codex", false, "unused")
	var cleanupOnly *wakeDetachedCleanupOnlyError
	if !errors.As(err, &cleanupOnly) {
		t.Fatalf("detached stale prepare error = %v, want cleanup-only refusal", err)
	}
	if _, statErr := os.Stat(filepath.Join(detachedPath, ".wake.lock")); !os.IsNotExist(statErr) {
		t.Fatalf("detached stale lock = %v, want absent", statErr)
	}
	assertWakeCheckTreeUnchanged(t, agentPath, successorBefore)
}

func TestPrepareCoopWakeLockProceedsAfterDiagnosticCleanupFailure(t *testing.T) {
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{PID: 66121, Generation: "stale"})
	diagnosticPath := filepath.Join(filepath.Dir(lockPath), wakeSelfUpgradeFileName)
	if err := os.Mkdir(diagnosticPath, 0o700); err != nil {
		t.Fatalf("create unremovable diagnostic residue: %v", err)
	}
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo { return wakeProcessInfo{PID: pid} })

	stderr := captureWakeStderr(t, func() {
		if err := prepareCoopWakeLock(root, "codex", false, "unused"); err != nil {
			t.Fatalf("prepare stale wake lock with diagnostic residue: %v", err)
		}
	})
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("stale lock remains: %v", err)
	}
	if info, err := os.Stat(diagnosticPath); err != nil || !info.IsDir() {
		t.Fatalf("diagnostic residue = (%v, %v), want retained directory", info, err)
	}
	if !strings.Contains(stderr, "left diagnostic-only self-upgrade residue") {
		t.Fatalf("cleanup warning missing residue: %q", stderr)
	}
}

func TestPrepareCoopWakeLockUnverifiedYesRemovesMetadataWithoutSignal(t *testing.T) {
	root := secureTempDirForTest(t)
	lockPath := writeUnverifiedCoopWakeLock(t, root)
	stderr := captureWakeStderr(t, func() {
		if err := prepareCoopWakeLock(root, "codex", true, "unused"); err != nil {
			t.Fatalf("approved cleanup: %v", err)
		}
	})
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("unverified lock remains: %v", err)
	}
	if !strings.Contains(stderr, "without signaling it") || !strings.Contains(stderr, "duplicate notifications may continue") {
		t.Fatalf("warning missing safety facts: %q", stderr)
	}
	if got := strings.Count(stderr, "duplicate notifications may continue"); got != 1 {
		t.Fatalf("duplicate-notification warning count = %d, want 1: %q", got, stderr)
	}
}

func TestPrepareCoopWakeLockDetachedUnverifiedCleansOldLockOnly(t *testing.T) {
	root := secureTempDirForTest(t)
	lockPath := writeUnverifiedCoopWakeLock(t, root)
	agentPath := filepath.Dir(lockPath)
	var detachedPath string
	var successorBefore map[string]wakeCheckTreeEntry
	detached := false
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if !detached {
			detached = true
			detachedPath = detachGenericWakeAgentDirForTest(t, agentPath, ".wake.lock")
			successorBefore = snapshotWakeCheckTree(t, agentPath)
		}
		return wakeProcessInfo{PID: pid}
	})

	err := prepareCoopWakeLock(root, "codex", true, "unused")
	var cleanupOnly *wakeDetachedCleanupOnlyError
	if !errors.As(err, &cleanupOnly) {
		t.Fatalf("detached unverified prepare error = %v, want cleanup-only refusal", err)
	}
	if _, statErr := os.Stat(filepath.Join(detachedPath, ".wake.lock")); !os.IsNotExist(statErr) {
		t.Fatalf("detached unverified lock = %v, want absent", statErr)
	}
	assertWakeCheckTreeUnchanged(t, agentPath, successorBefore)
}

func TestResolveMissingWakeLockAfterTerminationPreservesPresentGenerationError(t *testing.T) {
	root := secureTempDirForTest(t)
	writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:        66121,
		Generation: "same-generation",
	})
	inspection := inspectWakeLock(root, "codex")
	terminationErr := errors.New("test termination failure")

	retired, err := resolveMissingWakeLockAfterTermination(inspection, terminationErr)
	if retired {
		t.Fatal("present exact generation reported retired")
	}
	if !errors.Is(err, terminationErr) {
		t.Fatalf("present exact generation error = %v, want %v", err, terminationErr)
	}
}

func TestResolveMissingWakeLockAfterTerminationReturnsRetryForReplacementGeneration(t *testing.T) {
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:        66121,
		Generation: "old-generation",
	})
	inspection := inspectWakeLock(root, "codex")
	writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:        77121,
		Generation: "replacement-generation",
	})

	retired, err := resolveMissingWakeLockAfterTermination(
		inspection,
		errors.New("test termination failure"),
	)
	if err != nil {
		t.Fatalf("replacement-generation result = %v, want caller retry", err)
	}
	if retired {
		t.Fatal("replacement generation reported retired")
	}
	current := inspectWakeLock(root, "codex")
	if !current.Exists || current.Lock.Generation != "replacement-generation" {
		t.Fatalf("replacement lock changed at %s: %#v", lockPath, current)
	}
}

func TestResolveMissingWakeLockAfterTerminationDetachedSuccessorReturnsRetry(t *testing.T) {
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:        66121,
		Generation: "old-generation",
	})
	agentPath := filepath.Dir(lockPath)
	firstInspection := true
	var successorBefore map[string]wakeCheckTreeEntry
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if firstInspection {
			firstInspection = false
			return wakeProcessInfo{PID: pid}
		}
		detachGenericWakeAgentDirForTest(t, agentPath, ".wake.lock")
		successorBefore = snapshotWakeCheckTree(t, agentPath)
		return wakeProcessInfo{PID: pid}
	})
	inspection := inspectWakeLock(root, "codex")
	terminationErr := errors.New("termination outcome is unknown")

	retired, err := resolveMissingWakeLockAfterTermination(inspection, terminationErr)
	if retired || err != nil {
		t.Fatalf("detached successor result retired=%t err=%v, want retry", retired, err)
	}
	assertWakeCheckTreeUnchanged(t, agentPath, successorBefore)
}

func TestPrepareCoopWakeLockProvenForeignProcessRefusesWithoutMutation(t *testing.T) {
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{PID: 66121, ProcessStart: "start", BootID: "boot", Executable: "/opt/homebrew/bin/amq", Generation: "foreign"})
	before, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: true, StartToken: "start", BootID: "boot", Executable: "/bin/sleep", Args: []string{"/bin/sleep", "100"}}
	})
	if err := prepareCoopWakeLock(root, "codex", true, "unused"); err == nil || !strings.Contains(err.Error(), "proven not to be this wake") {
		t.Fatalf("foreign process result = %v", err)
	}
	after, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("foreign process lock changed")
	}
}

func TestPrepareCoopWakeLockLiveAuthoritativeRefusesWithoutMutation(t *testing.T) {
	const wakePID = 66121
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "authoritative-coop-conflict-injector")
	owner := currentAuthoritativeOwnerForCoopWakeTest(t)
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"exec"})
	target.Owner = &owner
	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatalf("write wake target: %v", err)
	}
	wakeArgs := []string{
		"/opt/homebrew/bin/amq",
		"wake",
		"--root",
		root,
		"--me",
		"codex",
		"--inject-via",
		injector,
	}
	lock := bindWakeLockToTarget(wakeLock{
		PID:          wakePID,
		Root:         canonicalWakeRoot(root),
		Agent:        "codex",
		TTY:          "/dev/ttys042",
		ProcessStart: owner.ProcessStart,
		BootID:       owner.BootID,
		Executable:   wakeArgs[0],
		Args:         wakeArgs,
		Generation:   "authoritative-conflict",
		OwnerSchema:  wakeOwnerLockSchema,
		Owner:        &owner,
	}, target)
	lock.WakeMode = wakeOwnerWakeMode
	lockPath := writeWakeLockExactForTest(t, root, "codex", lock)
	if err := os.Chmod(lockPath, wakeOwnerLockFileMode); err != nil {
		t.Fatal(err)
	}
	targetPath := wakeTargetPath(root, "codex")
	beforeLock, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeTarget, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: owner.ProcessStart,
			BootID:     owner.BootID,
			Executable: wakeArgs[0],
			Args:       wakeArgs,
		}
	})
	stubSignalWakeProcess(t, func(int, os.Signal) error {
		t.Fatal("authoritative wake must not be signaled through coop startup")
		return nil
	})
	ownerState := wakeOwnerSame
	var observeErr error
	observationClosed := false
	oldObserve := observeAuthoritativeWakeOwner
	observeAuthoritativeWakeOwner = func(got wakeOwner) (wakeOwnerObservation, error) {
		if !sameWakeOwner(&got, &owner) {
			t.Fatalf("observed owner = %#v, want %#v", got, owner)
		}
		if observeErr != nil {
			monitor := newWakeOwnerObservationMonitor(func() error {
				observationClosed = true
				return nil
			})
			monitor.finish(nil)
			return wakeOwnerObservation{State: wakeOwnerUnknown, monitor: monitor}, observeErr
		}
		return wakeOwnerObservation{State: ownerState}, nil
	}
	t.Cleanup(func() { observeAuthoritativeWakeOwner = oldObserve })

	err = prepareCoopWakeLock(root, "codex", true, "unused")
	if err == nil || !strings.Contains(err.Error(), "owned by a live process") {
		t.Fatalf("prepare live authoritative wake = %v, want live-owner refusal", err)
	}
	if strings.Contains(err.Error(), "doctor") ||
		strings.Contains(err.Error(), "wake retire") ||
		!strings.Contains(err.Error(), "pid:") ||
		!strings.Contains(err.Error(), "tty:") ||
		!strings.Contains(err.Error(), "started:") {
		t.Fatalf("live-owner refusal has unsafe or incomplete remedy: %v", err)
	}
	afterLock, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("authoritative lock changed: %v", err)
	}
	afterTarget, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("authoritative target changed: %v", err)
	}
	if string(afterLock) != string(beforeLock) || string(afterTarget) != string(beforeTarget) {
		t.Fatal("authoritative claim changed during startup refusal")
	}
	info, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != wakeOwnerLockFileMode {
		t.Fatalf("authoritative lock mode = %o, want %o", got, wakeOwnerLockFileMode)
	}

	ownerState = wakeOwnerDead
	if err := prepareCoopWakeLock(root, "codex", true, "unused"); err != nil {
		t.Fatalf("dead-owner authoritative wake blocked automatic takeover: %v", err)
	}
	afterDeadOwnerLock, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("dead-owner preflight changed authoritative lock: %v", err)
	}
	afterDeadOwnerTarget, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("dead-owner preflight changed authoritative target: %v", err)
	}
	if string(afterDeadOwnerLock) != string(beforeLock) ||
		string(afterDeadOwnerTarget) != string(beforeTarget) {
		t.Fatal("dead-owner preflight mutated authoritative claim before acquisition")
	}

	observeErr = errors.New("owner observer failed")
	if err := prepareCoopWakeLock(root, "codex", true, "unused"); err == nil {
		t.Fatal("owner observation failure did not block startup")
	}
	if !observationClosed {
		t.Fatal("owner observation failure leaked its returned capability")
	}
}

func TestPrepareCoopWakeLockHeadlessPrintsRemedyWithoutPrompt(t *testing.T) {
	root := secureTempDirForTest(t)
	lockPath := writeUnverifiedCoopWakeLock(t, root)
	remedy := "amq coop exec -y --root /resolved/session --me codex codex"
	stdout, stderr, got := captureEnvOutput(t, func() error {
		return prepareCoopWakeLock(root, "codex", false, remedy)
	})
	if got == nil || !strings.Contains(got.Error(), "declined") {
		t.Fatalf("headless result = %v", got)
	}
	if strings.Contains(stdout, "Clear it and start a fresh wake?") ||
		strings.Contains(stderr, "Clear it and start a fresh wake?") {
		t.Fatalf("headless cleanup printed an unanswerable prompt: stdout=%q stderr=%q", stdout, stderr)
	}
	inspectCommand := doctorRootCommandForOS(root, "", runtime.GOOS, "--ops")
	if !strings.Contains(stderr, remedy) || !strings.Contains(stderr, inspectCommand) || strings.Contains(stderr, "AM_ROOT=") {
		t.Fatalf("remedy missing: %q", stderr)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("headless cleanup changed lock: %v", err)
	}
}

func TestCoopWakeRemedyQuotesExplicitDoctorRoot(t *testing.T) {
	root := `/tmp/AMQ & peer's $root`
	got := coopWakeRemedy(wakeLockInspection{
		Agent:    "codex",
		LockPath: "/tmp/wake.lock",
		Root:     root,
	}, "unverified", "amq coop exec -y")
	want := doctorRootCommandForOS(root, "", runtime.GOOS, "--ops")
	if !strings.Contains(got, want) || strings.Contains(got, "AM_ROOT=") {
		t.Fatalf("wake remedy = %q, want explicit command %q", got, want)
	}
}

func TestCoopWakeRemedyForCommandUsesResolvedRootAndYes(t *testing.T) {
	got := coopWakeRemedyForCommand("/resolved/session", "codex", "codex", []string{"--dangerously-bypass-approvals-and-sandbox"})
	want := "amq coop exec -y --root /resolved/session --me codex codex --dangerously-bypass-approvals-and-sandbox"
	if got != want {
		t.Fatalf("command = %q, want %q", got, want)
	}
}

func TestWakeLockHasUsableNotificationPathChoosesExplicitTerminalFailDirection(t *testing.T) {
	tests := []struct {
		name       string
		tty        string
		known      bool
		has        bool
		wantUsable bool
	}{
		{name: "attached legacy unknown is usable", tty: "unknown", known: true, has: true, wantUsable: true},
		{name: "undeterminable legacy unknown fails closed", tty: "unknown"},
		{name: "undeterminable concrete path preserves Linux evidence", tty: "/dev/null", wantUsable: true},
		{name: "proven gone fails closed", tty: "/dev/amq-missing-notification-tty", known: true, has: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inspection := wakeLockInspection{
				Lock: wakeLock{TTY: tt.tty, WakeMode: wakeInjectModeRaw},
				Process: wakeProcessInfo{
					ControllingTerminalKnown: tt.known,
					HasControllingTerminal:   tt.has,
				},
			}
			if got := wakeLockHasUsableNotificationPath(inspection); got != tt.wantUsable {
				t.Fatalf("usable = %v, want %v", got, tt.wantUsable)
			}
		})
	}
}

func writeUnverifiedCoopWakeLock(t *testing.T, root string) string {
	t.Helper()
	return writeWakeLockForTest(t, root, "codex", wakeLock{PID: 66121, TTY: "", Hostname: "definitely-not-this-host", Started: time.Now().Add(-8 * 24 * time.Hour).UTC().Format(time.RFC3339), Generation: "unverified"})
}
