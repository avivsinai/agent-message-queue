//go:build linux

package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func stubLinuxPidfd(t *testing.T, open func(int, int) (int, error), send func(int, unix.Signal, *unix.Siginfo, int) error, poll func(int, time.Duration) (bool, error)) {
	t.Helper()
	oldOpen := linuxPidfdOpen
	oldSend := linuxPidfdSendSignal
	oldPoll := linuxPidfdPoll
	oldClose := linuxPidfdClose
	linuxPidfdOpen = open
	linuxPidfdSendSignal = send
	linuxPidfdPoll = poll
	linuxPidfdClose = func(int) error { return nil }
	t.Cleanup(func() {
		linuxPidfdOpen = oldOpen
		linuxPidfdSendSignal = oldSend
		linuxPidfdPoll = oldPoll
		linuxPidfdClose = oldClose
	})
}

func TestTerminateWakePidfdKillsValidatedChildAndCannotSignalAfterExit(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	proc := inspectWakeProcessPlatform(cmd.Process.Pid)
	if !proc.Running || proc.PID != cmd.Process.Pid || proc.StartToken == "" {
		t.Fatalf("child identity was not validated: %#v", proc)
	}
	pidfd, err := linuxPidfdOpen(cmd.Process.Pid, 0)
	if err != nil {
		t.Fatalf("pidfd_open child: %v", err)
	}
	defer func() { _ = linuxPidfdClose(pidfd) }()

	if err := terminateWakePidfd(pidfd); err != nil {
		t.Fatalf("terminate child via pidfd: %v", err)
	}
	// Wait may report a normal exit when the child handles SIGTERM; pidfd
	// ESRCH below is the authoritative proof that the process is gone.
	_, _ = cmd.Process.Wait()
	if err := linuxPidfdSendSignal(pidfd, unix.SIGTERM, nil, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("signal retained pidfd after exit = %v, want ESRCH", err)
	}
}

func TestRetireDoesNotSignalRecycledPID(t *testing.T) {
	old := exec.Command("sleep", "30")
	if err := old.Start(); err != nil {
		t.Fatalf("start old child: %v", err)
	}
	oldWaited := false
	t.Cleanup(func() {
		if oldWaited {
			return
		}
		_ = old.Process.Kill()
		_ = old.Wait()
	})
	pidfd, err := linuxPidfdOpen(old.Process.Pid, 0)
	if err != nil {
		_ = old.Process.Kill()
		_, _ = old.Process.Wait()
		t.Fatalf("pidfd_open old child: %v", err)
	}
	defer func() { _ = linuxPidfdClose(pidfd) }()
	if err := linuxPidfdSendSignal(pidfd, unix.SIGKILL, nil, 0); err != nil {
		t.Fatalf("kill old child via pidfd: %v", err)
	}
	if exited, err := linuxPidfdPoll(pidfd, time.Second); err != nil || !exited {
		t.Fatalf("poll old child exit = (%v, %v), want exited", exited, err)
	}
	_, _ = old.Process.Wait()
	oldWaited = true

	replacement := exec.Command("sleep", "30")
	if err := replacement.Start(); err != nil {
		t.Fatalf("start replacement child: %v", err)
	}
	t.Cleanup(func() {
		_ = replacement.Process.Kill()
		_, _ = replacement.Process.Wait()
	})
	if err := linuxPidfdSendSignal(pidfd, unix.SIGTERM, nil, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("signal old pidfd after replacement start = %v, want ESRCH", err)
	}
	if err := replacement.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("replacement child was signaled through stale pidfd: %v", err)
	}
}

func TestTerminateFailsClosedWhenPidfdOpenIsUnsupported(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{
		PID: wakePID, TTY: "missing", ProcessStart: "start-1", BootID: "boot-1", Executable: "/usr/bin/amq",
	})
	inspectCalls := 0
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		inspectCalls++
		p := matchingLinuxWakeProcess(pid, root)
		p.Executable = "/usr/bin/not-amq"
		p.Args = []string{"not-amq"}
		return p
	})
	stubLinuxPidfd(t,
		func(pid, flags int) (int, error) { return -1, syscall.ENOSYS },
		func(int, unix.Signal, *unix.Siginfo, int) error { t.Fatal("must not signal without pidfd"); return nil },
		func(int, time.Duration) (bool, error) { t.Fatal("must not poll without pidfd"); return false, nil },
	)

	inspection := inspectWakeLock(root, "codex")
	replaced, err := terminateAndRemoveOrphanedWakeLock(inspection)
	if err == nil || !strings.Contains(err.Error(), "pidfd_open") {
		t.Fatalf("termination error = %v, want pidfd_open failure", err)
	}
	if replaced {
		t.Fatal("unsupported pidfd unexpectedly replaced lock")
	}
	if inspectCalls != 1 {
		t.Fatalf("process inspections = %d, want only initial inspection before pidfd_open failure", inspectCalls)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock was not preserved: %v", err)
	}
}

func TestTerminateTreatsPidfdESRCHAsProvenGone(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          wakePID,
		TTY:          "missing",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/usr/bin/amq",
		Generation:   "abcdef0123456789abcdef0123456789",
	})
	metadata := readWakeLockMetadata(root, "codex")
	if !metadata.Exists || metadata.Lock.Generation == "" {
		t.Fatalf("wake lock metadata = %#v", metadata)
	}
	setCLIVersionForTest(t, "0.57.3-test")
	candidate, err := captureCurrentWakeImageEvidence()
	if err != nil {
		t.Fatal(err)
	}
	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()
	restart := wakeRestartRecord{
		Schema:              wakeRestartSchemaV2,
		RequestID:           "0123456789abcdef0123456789abcdef",
		Status:              wakeRestartPending,
		Root:                canonicalWakeRoot(root),
		Agent:               "codex",
		Generation:          metadata.Lock.Generation,
		SuccessorGeneration: "fedcba9876543210fedcba9876543210",
		Owner:               validWakeResumeOwnerForTest(),
		Candidate:           candidate,
	}
	if err := withWakeMutationScopeInDir(agentDir, func(scope *wakeMutationScope) error {
		return writeWakeRestartRecordAt(scope, restart)
	}); err != nil {
		t.Fatal(err)
	}
	restartPath := filepath.Join(agentDir.path, wakeRestartFileName)
	restartRaw, err := os.ReadFile(restartPath)
	if err != nil {
		t.Fatal(err)
	}
	restartInfo, err := os.Lstat(restartPath)
	if err != nil {
		t.Fatal(err)
	}
	inspectCalls := 0
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		inspectCalls++
		return matchingLinuxWakeProcess(pid, root)
	})
	stubLinuxPidfd(t,
		func(pid, flags int) (int, error) { return -1, syscall.ESRCH },
		func(int, unix.Signal, *unix.Siginfo, int) error {
			t.Fatal("must not signal a proven-gone process")
			return nil
		},
		func(int, time.Duration) (bool, error) {
			t.Fatal("must not poll a proven-gone process")
			return false, nil
		},
	)

	inspection := inspectWakeLock(root, "codex")
	replaced, err := terminateAndRemoveOrphanedWakeLock(inspection)
	if err != nil || !replaced {
		t.Fatalf("proven-gone replacement = (%v, %v), want (true, nil)", replaced, err)
	}
	if inspectCalls != 1 {
		t.Fatalf("process inspections = %d, want no PID re-lookup after ESRCH", inspectCalls)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("proven-gone lock was not removed: %v", err)
	}
	if _, err := os.Lstat(restartPath); !os.IsNotExist(err) {
		t.Fatalf("proven-gone restart record was not reclaimed: %v", err)
	}
	assertExactWakeQuarantineForTest(
		t,
		agentDir.path,
		wakeRestartFileName+".quarantined.",
		restartRaw,
		restartInfo,
	)
}

func TestTerminateOpensPidfdBeforeIdentityInspectionAndReleasesGuardBeforeWait(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	writeWakeLockForTest(t, root, "codex", wakeLock{
		PID: wakePID, TTY: "/dev/amq-missing-auto-replacement-tty", ProcessStart: "start-1", BootID: "boot-1", Executable: "/usr/bin/amq",
	})
	var mu sync.Mutex
	var events []string
	record := func(event string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event)
	}
	inspectCalls := 0
	releasePoll := make(chan struct{})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		inspectCalls++
		if inspectCalls > 1 {
			record("inspect")
		}
		select {
		case <-releasePoll:
			return wakeProcessInfo{PID: pid, Running: false}
		default:
			return matchingLinuxWakeProcess(pid, root)
		}
	})
	pollEntered := make(chan struct{})
	stubLinuxPidfd(t,
		func(pid, flags int) (int, error) { record("open"); return 99, nil },
		func(fd int, sig unix.Signal, info *unix.Siginfo, flags int) error { return nil },
		func(fd int, timeout time.Duration) (bool, error) {
			close(pollEntered)
			<-releasePoll
			return true, nil
		},
	)

	inspection := inspectWakeLock(root, "codex")
	done := make(chan error, 1)
	go func() {
		_, err := terminateAndRemoveOrphanedWakeLock(inspection)
		done <- err
	}()
	select {
	case <-pollEntered:
	case <-time.After(time.Second):
		t.Fatal("pidfd poll was not reached")
	}

	guardAcquired := make(chan error, 1)
	go func() {
		guardAcquired <- withWakeLifecycleGuard(root, "codex", func() error { return nil })
	}()
	select {
	case err := <-guardAcquired:
		if err != nil {
			t.Fatalf("acquire lifecycle guard during pidfd wait: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("lifecycle guard remained held during pidfd wait")
	}
	close(releasePoll)
	if err := <-done; err != nil {
		t.Fatalf("terminate after poll release: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) < 2 || events[0] != "open" || events[1] != "inspect" {
		t.Fatalf("pre-signal events = %v, want pidfd open before identity inspection", events)
	}
}

func TestTerminateWakePidfdRefusesWhenLifecycleGuardIsHeld(t *testing.T) {
	root := secureTempDirForTest(t)
	writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          4242,
		TTY:          "unknown",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/usr/bin/amq",
		Generation:   "guard-held-generation",
	})
	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()
	if err := withWakeLifecycleGuardInDir(agentDir, func(int) error { return nil }); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- withExistingWakeLifecycleGuardInDir(agentDir, func(int) error {
			close(entered)
			<-release
			return nil
		})
	}()
	defer releaseOnce.Do(func() { close(release) })

	oldSend := linuxPidfdSendSignal
	oldPoll := linuxPidfdPoll
	signalCalls := 0
	linuxPidfdSendSignal = func(int, unix.Signal, *unix.Siginfo, int) error {
		signalCalls++
		return nil
	}
	linuxPidfdPoll = func(int, time.Duration) (bool, error) {
		t.Fatal("guard-held termination polled without signal authorization")
		return false, nil
	}
	t.Cleanup(func() {
		linuxPidfdSendSignal = oldSend
		linuxPidfdPoll = oldPoll
	})

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("lifecycle guard holder did not enter")
	}
	// A nonblocking acquisition retries for a bounded interval. The call must
	// return refusal without reaching the pidfd effect.
	expected := inspectWakeLock(root, "codex")
	done := make(chan error, 1)
	go func() {
		_, err := terminateWakePidfdWithAuthorization(agentDir, expected, nil, false, 77)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "held by another process") {
			t.Fatalf("guard-held termination error = %v, want bounded guard refusal", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("guard-held termination did not return within its bounded retry window")
	}
	if signalCalls != 0 {
		t.Fatalf("guard-held termination signaled %d times", signalCalls)
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-holderDone; err != nil {
		t.Fatalf("release lifecycle guard holder: %v", err)
	}
}

func TestTerminateOrphanedWakeRefusesWhenLifecycleGuardIsHeld(t *testing.T) {
	root := secureTempDirForTest(t)
	writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          4242,
		TTY:          "unknown",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/usr/bin/amq",
		WakeMode:     wakeInjectModeRaw,
		Generation:   "top-level-guard-held-generation",
	})

	guardEntered := make(chan struct{})
	guardRelease := make(chan struct{})
	guardDone := make(chan error, 1)
	var releaseOnce sync.Once
	releaseGuard := func() { releaseOnce.Do(func() { close(guardRelease) }) }
	t.Cleanup(releaseGuard)
	go func() {
		guardDone <- withWakeLifecycleGuard(root, "codex", func() error {
			close(guardEntered)
			<-guardRelease
			return nil
		})
	}()
	select {
	case <-guardEntered:
	case <-time.After(time.Second):
		t.Fatal("lifecycle guard holder did not enter")
	}

	inspection := inspectWakeLock(root, "codex")
	done := make(chan struct {
		replaced bool
		err      error
	}, 1)
	go func() {
		replaced, err := terminateAndRemoveOrphanedWakeLock(inspection)
		done <- struct {
			replaced bool
			err      error
		}{replaced: replaced, err: err}
	}()
	select {
	case got := <-done:
		if got.err == nil || got.replaced || !strings.Contains(got.err.Error(), "held by another process") {
			t.Fatalf("top-level held-guard termination = (%v,%v), want bounded refusal", got.replaced, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("top-level termination remained blocked on the lifecycle guard")
	}
	releaseGuard()
	if err := <-guardDone; err != nil {
		t.Fatalf("release lifecycle guard holder: %v", err)
	}
}

func TestTerminateWakePidfdRefusesSamePIDReplacementGeneration(t *testing.T) {
	root := secureTempDirForTest(t)
	lock := wakeLock{
		PID:          4242,
		TTY:          "unknown",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/usr/bin/amq",
		Generation:   "original-generation",
	}
	writeWakeLockForTest(t, root, "codex", lock)
	expected := inspectWakeLock(root, "codex")
	lock.Generation = "replacement-generation"
	writeWakeLockForTest(t, root, "codex", lock)

	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()
	if err := withWakeLifecycleGuardInDir(agentDir, func(int) error { return nil }); err != nil {
		t.Fatal(err)
	}

	oldSend := linuxPidfdSendSignal
	oldPoll := linuxPidfdPoll
	signalCalls := 0
	linuxPidfdSendSignal = func(int, unix.Signal, *unix.Siginfo, int) error {
		signalCalls++
		return nil
	}
	linuxPidfdPoll = func(int, time.Duration) (bool, error) {
		t.Fatal("generation-mismatched termination polled without signal authorization")
		return false, nil
	}
	t.Cleanup(func() {
		linuxPidfdSendSignal = oldSend
		linuxPidfdPoll = oldPoll
	})

	attempted, err := terminateWakePidfdWithAuthorization(agentDir, expected, nil, false, 77)
	if err == nil || !strings.Contains(err.Error(), "generation changed") {
		t.Fatalf("same-PID replacement error = %v, want generation refusal", err)
	}
	if attempted {
		t.Fatal("same-PID replacement reported an attempted signal")
	}
	if signalCalls != 0 {
		t.Fatalf("same-PID replacement signaled %d times", signalCalls)
	}
}

func TestTerminateWakePidfdRefusesWhenWakeLockDisappears(t *testing.T) {
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          4242,
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/usr/bin/amq",
		Generation:   "absent-lock-generation",
	})
	expected := inspectWakeLock(root, "codex")
	if err := os.Remove(lockPath); err != nil {
		t.Fatalf("remove wake lock before signal authorization: %v", err)
	}

	var signalCalls, pollCalls int
	stubLinuxPidfd(
		t,
		func(int, int) (int, error) { return 77, nil },
		func(int, unix.Signal, *unix.Siginfo, int) error {
			signalCalls++
			return nil
		},
		func(int, time.Duration) (bool, error) {
			pollCalls++
			return true, nil
		},
	)

	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()

	attempted, err := terminateWakePidfdWithAuthorization(agentDir, expected, nil, true, 77)
	if err == nil || !strings.Contains(err.Error(), "wake lock is missing before SIGTERM") {
		t.Fatalf("missing-lock authorization error = %v, want refusal", err)
	}
	if attempted {
		t.Fatal("missing-lock authorization reported an attempted signal")
	}
	if signalCalls != 0 {
		t.Fatalf("missing-lock authorization sent %d signals", signalCalls)
	}
	if pollCalls != 0 {
		t.Fatalf("missing-lock authorization polled %d times", pollCalls)
	}
}

func TestRetireWakeRefusesMissingLockBeforeSignal(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	requested, lockPath := installRetireWakeFixture(t, root, "codex", injector, []string{"exec", "terminal-a"}, wakePID)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return matchingRetireWakeProcess(pid, root, "codex", injector)
	})

	var signalCalls, pollCalls int
	stubLinuxPidfd(
		t,
		func(pid, flags int) (int, error) {
			if pid != wakePID || flags != 0 {
				t.Fatalf("pidfd_open = (%d, %d), want (%d, 0)", pid, flags, wakePID)
			}
			if err := os.Remove(lockPath); err != nil {
				t.Fatalf("remove lock during pidfd authorization: %v", err)
			}
			return 99, nil
		},
		func(int, unix.Signal, *unix.Siginfo, int) error {
			signalCalls++
			return nil
		},
		func(int, time.Duration) (bool, error) {
			pollCalls++
			return true, nil
		},
	)

	result, err := retireWake(root, "codex", requested)
	if err == nil || result.Status != "refused" || !strings.Contains(result.Reason, "wake lock is missing before SIGTERM") {
		t.Fatalf("missing-lock retirement = %#v err=%v, want pre-signal refusal", result, err)
	}
	if signalCalls != 0 {
		t.Fatalf("missing-lock retirement sent %d signals", signalCalls)
	}
	if pollCalls != 0 {
		t.Fatalf("missing-lock retirement polled %d times", pollCalls)
	}
}

func TestRetireWakeRefusesSuccessorGenerationBeforeSignal(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "injector")
	requested, lockPath := installRetireWakeFixture(t, root, "codex", injector, []string{"exec", "terminal-a"}, wakePID)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return matchingRetireWakeProcess(pid, root, "codex", injector)
	})

	var signalCalls, pollCalls int
	stubLinuxPidfd(
		t,
		func(pid, flags int) (int, error) {
			if pid != wakePID || flags != 0 {
				t.Fatalf("pidfd_open = (%d, %d), want (%d, 0)", pid, flags, wakePID)
			}
			current := inspectWakeLock(root, "codex").Lock
			current.Generation = "successor-generation"
			writeWakeLockForTest(t, root, "codex", current)
			return 99, nil
		},
		func(int, unix.Signal, *unix.Siginfo, int) error {
			signalCalls++
			return nil
		},
		func(int, time.Duration) (bool, error) {
			pollCalls++
			return true, nil
		},
	)

	result, err := retireWake(root, "codex", requested)
	if err == nil || result.Status != "refused" || !strings.Contains(result.Reason, "wake lock generation changed before SIGTERM") {
		t.Fatalf("successor-generation retirement = %#v err=%v, want pre-signal refusal", result, err)
	}
	if signalCalls != 0 {
		t.Fatalf("successor-generation retirement sent %d signals", signalCalls)
	}
	if pollCalls != 0 {
		t.Fatalf("successor-generation retirement polled %d times", pollCalls)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("successor lock disappeared after refusal: %v", err)
	}
}

func TestTerminateRefusesLiveRawUnknownTerminalBeforePidfdSignal(t *testing.T) {
	const (
		wakePID = 4242
		pidfd   = 99
	)
	root := secureTempDirForTest(t)
	lockPath := writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          wakePID,
		TTY:          "unknown",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/usr/bin/amq",
		Args:         []string{"/usr/bin/amq", "wake", "--root", root, "--me", "codex"},
		WakeMode:     wakeInjectModeRaw,
		Generation:   "live-raw-unknown-terminal",
	})
	before, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read original lock: %v", err)
	}
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return matchingLinuxWakeProcess(pid, root)
	})

	openCalls := 0
	signalCalls := 0
	pollCalls := 0
	stubLinuxPidfd(
		t,
		func(pid, flags int) (int, error) {
			openCalls++
			if pid != wakePID || flags != 0 {
				t.Fatalf("pidfd_open = (%d, %d), want (%d, 0)", pid, flags, wakePID)
			}
			return pidfd, nil
		},
		func(gotFD int, signal unix.Signal, _ *unix.Siginfo, flags int) error {
			signalCalls++
			return nil
		},
		func(gotFD int, timeout time.Duration) (bool, error) {
			pollCalls++
			return true, nil
		},
	)

	inspection := inspectWakeLock(root, "codex")
	if inspection.Status != wakeLockValid || !inspection.IdentityConfirmed {
		t.Fatalf("initial inspection = %#v, want identity-confirmed valid wake", inspection)
	}
	replaced, err := terminateAndRemoveOrphanedWakeLock(inspection)
	if err == nil || !strings.Contains(err.Error(), "refusing to signal without consent") {
		t.Errorf("termination error = %v, want live-raw refusal", err)
	}
	if replaced {
		t.Error("live raw wake without replacement evidence was replaced")
	}
	if openCalls != 1 {
		t.Errorf("pidfd open calls = %d, want 1 identity capability acquisition", openCalls)
	}
	if signalCalls != 0 {
		t.Errorf("pidfd signal calls = %d, want 0", signalCalls)
	}
	if pollCalls != 0 {
		t.Errorf("pidfd poll calls = %d, want 0", pollCalls)
	}
	after, readErr := os.ReadFile(lockPath)
	if readErr != nil {
		t.Fatalf("read preserved lock: %v", readErr)
	}
	if string(after) != string(before) {
		t.Error("live raw refusal changed the exact wake lock")
	}
}

func matchingLinuxWakeProcess(pid int, root string) wakeProcessInfo {
	return wakeProcessInfo{
		PID: pid, Running: true, StartToken: "start-1", BootID: "boot-1", Executable: "/usr/bin/amq",
		Args: []string{"/usr/bin/amq", "wake", "--root", root, "--me", "codex"},
	}
}

func TestTerminateWakePidfdKillsChildThatIgnoresSIGTERM(t *testing.T) {
	cmd := exec.Command("sh", "-c", "trap '' TERM; exec sleep 30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	pidfd, err := linuxPidfdOpen(cmd.Process.Pid, 0)
	if err != nil {
		t.Fatalf("pidfd_open child: %v", err)
	}
	defer func() { _ = linuxPidfdClose(pidfd) }()
	if err := terminateWakePidfd(pidfd); err != nil {
		t.Fatalf("terminate TERM-immune child: %v", err)
	}
	_, _ = cmd.Process.Wait()
	if err := linuxPidfdSendSignal(pidfd, unix.SIGTERM, nil, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("signal retained pidfd after exit = %v, want ESRCH", err)
	}
}

func TestTerminateWakePidfdReportsRetiredWhenSIGKILLExitIsDelayed(t *testing.T) {
	var timeouts []time.Duration
	stubLinuxPidfd(t,
		func(int, int) (int, error) { return 7, nil },
		func(int, unix.Signal, *unix.Siginfo, int) error { return nil },
		func(_ int, timeout time.Duration) (bool, error) {
			timeouts = append(timeouts, timeout)
			return timeout == wakeTerminateKillConfirm, nil
		},
	)
	if err := terminateWakePidfd(7); err != nil {
		t.Fatalf("delayed SIGKILL exit should retire: %v", err)
	}
	if len(timeouts) != 2 || timeouts[0] != wakeTerminateGrace || timeouts[1] != wakeTerminateKillConfirm {
		t.Fatalf("pidfd poll timeouts = %v, want [%s %s]", timeouts, wakeTerminateGrace, wakeTerminateKillConfirm)
	}
}

func TestTerminateWakePidfdRefusesImmortalWithinKillConfirm(t *testing.T) {
	var timeouts []time.Duration
	killPollReportedAlive := false
	stubLinuxPidfd(t,
		func(int, int) (int, error) { return 7, nil },
		func(int, unix.Signal, *unix.Siginfo, int) error { return nil },
		func(_ int, timeout time.Duration) (bool, error) {
			timeouts = append(timeouts, timeout)
			if timeout == wakeTerminateKillConfirm {
				killPollReportedAlive = true
			}
			return false, nil
		},
	)
	err := terminateWakePidfd(7)
	if err == nil || !strings.Contains(err.Error(), "still alive after SIGKILL") {
		t.Fatalf("immortal process error = %v, want SIGKILL confirmation refusal", err)
	}
	if !killPollReportedAlive {
		t.Fatal("refused before the SIGKILL poll reported not-exited")
	}
	if len(timeouts) != 2 || timeouts[0] != wakeTerminateGrace || timeouts[1] != wakeTerminateKillConfirm {
		t.Fatalf("pidfd poll timeouts = %v, want [%s %s]", timeouts, wakeTerminateGrace, wakeTerminateKillConfirm)
	}
}
