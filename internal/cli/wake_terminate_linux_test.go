//go:build linux

package cli

import (
	"errors"
	"os"
	"os/exec"
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
		PID: wakePID, TTY: "missing", ProcessStart: "start-1", BootID: "boot-1", Executable: "/usr/bin/amq",
	})
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
}

func TestTerminateOpensPidfdBeforeIdentityInspectionAndReleasesGuardBeforeWait(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	writeWakeLockForTest(t, root, "codex", wakeLock{
		PID: wakePID, TTY: "missing", ProcessStart: "start-1", BootID: "boot-1", Executable: "/usr/bin/amq",
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

func matchingLinuxWakeProcess(pid int, root string) wakeProcessInfo {
	return wakeProcessInfo{
		PID: pid, Running: true, StartToken: "start-1", BootID: "boot-1", Executable: "/usr/bin/amq",
		Args: []string{"/usr/bin/amq", "wake", "--root", root, "--me", "codex"},
	}
}
