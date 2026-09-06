//go:build linux

package cli

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"golang.org/x/sys/unix"
)

func TestLinuxOwnerObservationRetainsPidfdAcrossStableDoubleSnapshot(t *testing.T) {
	owner := wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	pidfdPipe := make([]int, 2)
	if err := unix.Pipe2(pidfdPipe, unix.O_CLOEXEC); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(pidfdPipe[1]) })
	var events []string
	oldOpen := linuxPidfdOpen
	oldPoll := linuxPidfdPoll
	oldClose := linuxPidfdClose
	linuxPidfdOpen = func(pid, flags int) (int, error) {
		events = append(events, "open")
		if pid != owner.PID || flags != 0 {
			t.Fatalf("pidfd_open(%d,%d)", pid, flags)
		}
		return pidfdPipe[0], nil
	}
	linuxPidfdPoll = func(fd int, _ time.Duration) (bool, error) {
		events = append(events, "poll")
		if fd != pidfdPipe[0] {
			t.Fatalf("poll fd = %d", fd)
		}
		return false, nil
	}
	linuxPidfdClose = func(fd int) error {
		events = append(events, "close")
		if fd != pidfdPipe[0] {
			return fmt.Errorf("close fd = %d, want %d", fd, pidfdPipe[0])
		}
		return unix.Close(fd)
	}
	t.Cleanup(func() {
		linuxPidfdOpen = oldOpen
		linuxPidfdPoll = oldPoll
		linuxPidfdClose = oldClose
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		events = append(events, "inspect")
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: owner.ProcessStart,
			BootID:     owner.BootID,
		}
	})
	stubWakeProcessSID(t, func(pid int) (int, error) {
		events = append(events, "session")
		return owner.SessionID, nil
	})

	observation, err := observeAuthoritativeWakeOwnerPlatform(owner)
	if err != nil || observation.State != wakeOwnerSame {
		t.Fatalf("observation = %#v err=%v", observation, err)
	}
	t.Cleanup(func() { _ = observation.Close() })
	if observation.Done() == nil {
		t.Fatal("same-owner observation has no lifetime signal")
	}
	select {
	case <-observation.Done():
		t.Fatal("same-owner observation ended before owner exit or explicit disposal")
	default:
	}
	if len(events) == 0 || events[0] != "open" {
		t.Fatalf("events before close = %v, want pidfd open first", events)
	}
	if strings.Contains(strings.Join(events, ","), "close") {
		t.Fatalf("owner pidfd closed before caller completed guarded work: %v", events)
	}
	if err := observation.Close(); err != nil {
		t.Fatal(err)
	}
	if got := events[len(events)-1]; got != "close" {
		t.Fatalf("last event = %q, events=%v", got, events)
	}
	select {
	case <-observation.Done():
	default:
		t.Fatal("same-owner observation did not end after explicit disposal")
	}
}

func TestLinuxStableOwnerWakeStopNeverSignalsReusedNumericPID(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	injector := writeExecutableForTest(t, "linux-owner-stop-injector")
	owner := wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	target := mustNewWakeTargetForTest(t, root, "codex", injector, nil)
	target.Owner = &owner
	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatal(err)
	}
	lock := bindWakeLockToTarget(wakeLock{
		PID:          5151,
		TTY:          "unknown",
		ProcessStart: "67890",
		BootID:       owner.BootID,
		Executable:   "/usr/local/bin/amq",
		Args:         []string{"amq", "wake", "--root", root, "--me", "codex", "--inject-via", injector},
		Generation:   "linux-owner-stop-generation",
		OwnerSchema:  wakeOwnerLockSchema,
		Owner:        &owner,
	}, target)
	lock.WakeMode = wakeOwnerWakeMode
	lockPath := writeWakeLockForTest(t, root, "codex", lock)
	if err := os.Chmod(lockPath, wakeOwnerLockFileMode); err != nil {
		t.Fatal(err)
	}

	var events []string
	oldOpen := linuxPidfdOpen
	oldPoll := linuxPidfdPoll
	oldClose := linuxPidfdClose
	oldSignal := linuxPidfdSend
	linuxPidfdOpen = func(pid, flags int) (int, error) {
		events = append(events, "open")
		return 88, nil
	}
	linuxPidfdPoll = func(fd int, _ time.Duration) (bool, error) {
		events = append(events, "poll")
		return false, nil
	}
	linuxPidfdClose = func(fd int) error {
		events = append(events, "close")
		return nil
	}
	linuxPidfdSend = func(int, unix.Signal, *unix.Siginfo, int) error {
		t.Fatal("reused numeric PID received a signal")
		return nil
	}
	t.Cleanup(func() {
		linuxPidfdOpen = oldOpen
		linuxPidfdPoll = oldPoll
		linuxPidfdClose = oldClose
		linuxPidfdSend = oldSignal
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		events = append(events, "inspect")
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: "99999",
			BootID:     owner.BootID,
			Executable: "/usr/local/bin/amq",
			Args:       lock.Args,
		}
	})

	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()
	err = withWakeMutationScopeInDir(agentDir, func(scope *wakeMutationScope) error {
		dirfd, scopedAgentDir, err := scope.location()
		if err != nil {
			return err
		}
		agentDir = scopedAgentDir
		expected := readWakeLockMetadataAt(dirfd, agentDir, root, "codex")
		capability, err := prepareAuthoritativeWakeStopPlatform(scope, expected)
		if err != nil {
			return err
		}
		defer func() { _ = capability.Close() }()
		if !capability.Absent {
			t.Fatal("reused wake PID was treated as the recorded wake")
		}
		return capability.Stop(wakeOwnerReleaseAuthorization{})
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 || events[0] != "open" {
		t.Fatalf("stable stop events = %v, want pidfd open before inspection", events)
	}
}
