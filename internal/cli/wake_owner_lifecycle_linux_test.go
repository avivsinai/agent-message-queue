//go:build linux

package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"golang.org/x/sys/unix"
)

func TestLinuxOwnerWakeChildRequestsPidfdAtProcessCreation(t *testing.T) {
	oldOpen := linuxPidfdOpen
	oldClose := linuxPidfdClose
	linuxPidfdOpen = func(pid, flags int) (int, error) {
		if pid != os.Getpid() || flags != 0 {
			t.Fatalf("pidfd preflight = (%d,%d)", pid, flags)
		}
		return 71, nil
	}
	linuxPidfdClose = func(fd int) error {
		if fd != 71 && fd != 72 {
			t.Fatalf("unexpected close fd = %d", fd)
		}
		return nil
	}
	t.Cleanup(func() {
		linuxPidfdOpen = oldOpen
		linuxPidfdClose = oldClose
	})

	cmd := exec.Command("true")
	capability, err := prepareAuthoritativeWakeChildPlatform(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = capability.Close() }()
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.PidFD == nil {
		t.Fatal("owner wake child did not request an atomic pidfd")
	}
	*cmd.SysProcAttr.PidFD = 72
	if err := capability.Bind(&os.Process{Pid: 4242}); err != nil {
		t.Fatalf("bind atomic child pidfd: %v", err)
	}
}

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

func TestLinuxOwnerObservationTreatsPidfdConfirmedMidSnapshotExitAsDead(t *testing.T) {
	owner := wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	oldOpen := linuxPidfdOpen
	oldPoll := linuxPidfdPoll
	oldClose := linuxPidfdClose
	linuxPidfdOpen = func(pid, flags int) (int, error) {
		if pid != owner.PID || flags != 0 {
			t.Fatalf("pidfd_open(%d,%d)", pid, flags)
		}
		return 77, nil
	}
	polls := 0
	linuxPidfdPoll = func(fd int, _ time.Duration) (bool, error) {
		if fd != 77 {
			t.Fatalf("poll fd = %d", fd)
		}
		polls++
		return polls == 2, nil
	}
	linuxPidfdClose = func(fd int) error {
		if fd != 77 {
			t.Fatalf("close fd = %d", fd)
		}
		return nil
	}
	t.Cleanup(func() {
		linuxPidfdOpen = oldOpen
		linuxPidfdPoll = oldPoll
		linuxPidfdClose = oldClose
	})
	inspections := 0
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		inspections++
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: owner.ProcessStart,
			BootID:     owner.BootID,
		}
	})
	stubWakeProcessSID(t, func(int) (int, error) {
		return owner.SessionID, nil
	})

	observation, err := observeAuthoritativeWakeOwnerPlatform(owner)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = observation.Close() }()
	if observation.State != wakeOwnerDead {
		t.Fatalf("observation = %#v, want pidfd-confirmed dead", observation)
	}
	select {
	case <-observation.Done():
	default:
		t.Fatal("dead-owner observation Done is not already closed")
	}
	if inspections != 1 || polls != 2 {
		t.Fatalf("mid-snapshot exit inspected=%d polled=%d, want 1 and 2", inspections, polls)
	}
}

func TestLinuxOwnerObservationESRCHIsDeadAndAlreadyDone(t *testing.T) {
	oldOpen := linuxPidfdOpen
	linuxPidfdOpen = func(int, int) (int, error) {
		return -1, syscall.ESRCH
	}
	t.Cleanup(func() { linuxPidfdOpen = oldOpen })

	observation, err := observeAuthoritativeWakeOwnerPlatform(wakeOwner{PID: 4242})
	if err != nil || observation.State != wakeOwnerDead {
		t.Fatalf("observation = %#v err=%v, want dead", observation, err)
	}
	select {
	case <-observation.Done():
	default:
		t.Fatal("ESRCH observation Done is not already closed")
	}
}

func TestLinuxOwnerObservationUnsupportedHasNoDoneSignal(t *testing.T) {
	oldOpen := linuxPidfdOpen
	linuxPidfdOpen = func(int, int) (int, error) {
		return -1, syscall.ENOSYS
	}
	t.Cleanup(func() { linuxPidfdOpen = oldOpen })

	observation, err := observeAuthoritativeWakeOwnerPlatform(wakeOwner{PID: 4242})
	if err == nil || observation.State != wakeOwnerUnknown || !observation.CapabilityUnsupported {
		t.Fatalf("observation = %#v err=%v, want unsupported unknown", observation, err)
	}
	if observation.Done() != nil {
		t.Fatal("unsupported owner observation exposed a false lifetime signal")
	}
}

func TestLinuxOwnerObservationStableUnknownReturnsErrorAndNoDoneSignal(t *testing.T) {
	owner := wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	const pidfd = 77
	oldOpen := linuxPidfdOpen
	oldPoll := linuxPidfdPoll
	oldClose := linuxPidfdClose
	linuxPidfdOpen = func(pid, flags int) (int, error) {
		if pid != owner.PID || flags != 0 {
			t.Fatalf("pidfd_open(%d,%d)", pid, flags)
		}
		return pidfd, nil
	}
	polls := 0
	linuxPidfdPoll = func(fd int, _ time.Duration) (bool, error) {
		if fd != pidfd {
			t.Fatalf("poll fd = %d", fd)
		}
		polls++
		return false, nil
	}
	closes := 0
	linuxPidfdClose = func(fd int) error {
		if fd != pidfd {
			t.Fatalf("close fd = %d", fd)
		}
		closes++
		return nil
	}
	t.Cleanup(func() {
		linuxPidfdOpen = oldOpen
		linuxPidfdPoll = oldPoll
		linuxPidfdClose = oldClose
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: owner.ProcessStart,
			BootID:     owner.BootID,
		}
	})
	sessionErr := errors.New("session inspection denied")
	stubWakeProcessSID(t, func(int) (int, error) {
		return 0, sessionErr
	})

	observation, err := observeAuthoritativeWakeOwnerPlatform(owner)
	if err == nil || !strings.Contains(err.Error(), sessionErr.Error()) {
		t.Fatalf("unknown observation error = %v", err)
	}
	if observation.State != wakeOwnerUnknown {
		t.Fatalf("observation = %#v, want unknown", observation)
	}
	if observation.Done() != nil {
		t.Fatal("stable unknown observation exposed a false lifetime signal")
	}
	if polls != 3 || closes != 1 {
		t.Fatalf("stable unknown polled=%d closed=%d, want 3 and 1", polls, closes)
	}
}

func TestLinuxOwnerObservationMonitorClosesDoneOnOwnerExit(t *testing.T) {
	pidfdPipe := make([]int, 2)
	if err := unix.Pipe2(pidfdPipe, unix.O_CLOEXEC); err != nil {
		t.Fatal(err)
	}
	monitor, err := startLinuxWakeOwnerObservationMonitor(pidfdPipe[0])
	if err != nil {
		_ = unix.Close(pidfdPipe[0])
		_ = unix.Close(pidfdPipe[1])
		t.Fatal(err)
	}
	observation := wakeOwnerObservation{
		State:   wakeOwnerSame,
		monitor: monitor,
	}
	t.Cleanup(func() { _ = observation.Close() })
	if _, err := unix.Write(pidfdPipe[1], []byte{1}); err != nil {
		t.Fatal(err)
	}
	_ = unix.Close(pidfdPipe[1])

	select {
	case <-observation.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("owner terminal event did not close observation Done")
	}
	if err := observation.Close(); err != nil {
		t.Fatalf("close terminal observation: %v", err)
	}
}

func TestLinuxOwnerObservationCloseIsConcurrentAndIdempotent(t *testing.T) {
	pidfdPipe := make([]int, 2)
	if err := unix.Pipe2(pidfdPipe, unix.O_CLOEXEC); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(pidfdPipe[1]) })
	monitor, err := startLinuxWakeOwnerObservationMonitor(pidfdPipe[0])
	if err != nil {
		_ = unix.Close(pidfdPipe[0])
		t.Fatal(err)
	}
	observation := wakeOwnerObservation{
		State:   wakeOwnerSame,
		monitor: monitor,
	}

	const callers = 16
	errs := make(chan error, callers)
	var wait sync.WaitGroup
	wait.Add(callers)
	for range callers {
		go func() {
			defer wait.Done()
			errs <- observation.Close()
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent close: %v", err)
		}
	}
	select {
	case <-observation.Done():
	default:
		t.Fatal("explicit disposal did not close observation Done")
	}
}

func TestLinuxOwnerObservationMonitorFailureClosesDone(t *testing.T) {
	const invalidFD = 1 << 20
	monitor, err := startLinuxWakeOwnerObservationMonitor(invalidFD)
	if err != nil {
		t.Fatal(err)
	}
	observation := wakeOwnerObservation{
		State:   wakeOwnerSame,
		monitor: monitor,
	}
	select {
	case <-observation.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("monitor failure did not close observation Done")
	}
	if err := observation.Close(); err == nil {
		t.Fatal("monitor failure was not surfaced by Close")
	}
}

func TestLinuxUnsupportedPidfdStillCapturesCurrentOwnerButRejectsChildSupervision(t *testing.T) {
	oldOpen := linuxPidfdOpen
	linuxPidfdOpen = func(int, int) (int, error) {
		return -1, syscall.ENOSYS
	}
	t.Cleanup(func() { linuxPidfdOpen = oldOpen })

	const bootID = "11111111-1111-1111-1111-111111111111"
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid != os.Getpid() {
			t.Fatalf("inspect pid = %d, want current pid %d", pid, os.Getpid())
		}
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: "12345",
			BootID:     bootID,
		}
	})
	stubWakeProcessSID(t, func(pid int) (int, error) {
		if pid != os.Getpid() {
			t.Fatalf("session pid = %d, want current pid %d", pid, os.Getpid())
		}
		return 99, nil
	})

	owner, err := captureAuthoritativeCurrentWakeOwner()
	if err != nil {
		t.Fatalf("capture current owner without pidfd: %v", err)
	}
	if owner.PID != os.Getpid() || owner.ProcessStart != "12345" ||
		owner.BootID != bootID || owner.SessionID != 99 {
		t.Fatalf("captured owner = %#v", owner)
	}

	_, err = prepareAuthoritativeWakeChildPlatform(exec.Command("true"))
	var unsupported *wakeOwnerChildCapabilityUnsupportedError
	if !errors.As(err, &unsupported) {
		t.Fatalf("child capability error = %v, want unsupported supervision error", err)
	}
}

func TestLinuxFreshUnsupportedOwnerPidfdRefusesOwnerAcquisition(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	injector := writeExecutableForTest(t, "linux-owner-fallback-injector")
	owner := wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	target := mustNewWakeTargetForTest(t, root, "codex", injector, nil)
	target.Owner = &owner

	oldOpen := linuxPidfdOpen
	linuxPidfdOpen = func(pid, flags int) (int, error) {
		if pid == owner.PID {
			return -1, syscall.ENOSYS
		}
		return oldOpen(pid, flags)
	}
	t.Cleanup(func() { linuxPidfdOpen = oldOpen })
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: "67890",
			BootID:     owner.BootID,
			Executable: "/usr/local/bin/amq",
			Args:       []string{"amq", "wake", "--root", root, "--me", "codex", "--inject-via", injector},
		}
	})

	var acquireErr error
	stderr := captureWakeStderr(t, func() {
		_, acquireErr = acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
			target:   &target,
			wakeMode: wakeTargetInjectVia,
		})
	})
	if acquireErr == nil || !strings.Contains(acquireErr.Error(), "pidfd_open") {
		t.Fatalf("owner acquisition error = %v, want unsupported observer refusal", acquireErr)
	}
	if stderr != "" {
		t.Fatalf("owner acquisition emitted degradation warning: %q", stderr)
	}
	inspection := inspectWakeLock(root, "codex")
	if inspection.Exists {
		t.Fatalf("unsupported owner observation published a wake claim: %#v", inspection)
	}
	persisted, exists, err := readWakeTarget(root, "codex")
	if err != nil || exists {
		t.Fatalf("unsupported owner observation target = %#v exists=%v err=%v", persisted, exists, err)
	}
	if !sameWakeOwner(target.Owner, &owner) {
		t.Fatalf("owner acquisition mutated caller target owner: %#v", target.Owner)
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
	oldSignal := linuxPidfdSendSignal
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
	linuxPidfdSendSignal = func(int, unix.Signal, *unix.Siginfo, int) error {
		t.Fatal("reused numeric PID received a signal")
		return nil
	}
	t.Cleanup(func() {
		linuxPidfdOpen = oldOpen
		linuxPidfdPoll = oldPoll
		linuxPidfdClose = oldClose
		linuxPidfdSendSignal = oldSignal
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
	err = withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		expected := readWakeLockMetadataAt(dirfd, agentDir, root, "codex")
		capability, err := prepareAuthoritativeWakeStopPlatform(dirfd, agentDir, expected)
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

func TestLinuxMalformedOwnerWakeIdentityNeverSignalsMatchingArgvPID(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	injector := writeExecutableForTest(t, "linux-malformed-owner-stop-injector")
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
		ProcessStart: "",
		BootID:       owner.BootID,
		Executable:   "/usr/local/bin/amq",
		Args:         []string{"amq", "wake", "--root", root, "--me", "codex", "--inject-via", injector},
		Generation:   "linux-malformed-owner-stop-generation",
		OwnerSchema:  wakeOwnerLockSchema,
		Owner:        &owner,
	}, target)
	lock.WakeMode = wakeOwnerWakeMode
	lockPath := writeWakeLockForTest(t, root, "codex", lock)
	if err := os.Chmod(lockPath, wakeOwnerLockFileMode); err != nil {
		t.Fatal(err)
	}

	oldOpen := linuxPidfdOpen
	oldSignal := linuxPidfdSendSignal
	linuxPidfdOpen = func(int, int) (int, error) {
		t.Fatal("malformed authoritative identity reached pidfd_open")
		return -1, nil
	}
	linuxPidfdSendSignal = func(int, unix.Signal, *unix.Siginfo, int) error {
		t.Fatal("matching-argv PID received a signal from malformed owner lock")
		return nil
	}
	t.Cleanup(func() {
		linuxPidfdOpen = oldOpen
		linuxPidfdSendSignal = oldSignal
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: "99999",
			BootID:     owner.BootID,
			Executable: "/usr/local/bin/amq",
			Args:       lock.Args,
		}
	})

	inspection := inspectWakeLock(root, "codex")
	if inspection.Status != wakeLockUnverified {
		t.Fatalf("malformed owner wake status = %s, want unverified", inspection.Status)
	}
	state, _ := classifyWakeIdentity(inspection, inspection.Process)
	if state != wakeIdentityUnknown {
		t.Fatalf("matching-argv malformed owner wake identity = %s, want unknown", state)
	}

	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()
	err = withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		expected := readWakeLockMetadataAt(dirfd, agentDir, root, "codex")
		_, err := prepareAuthoritativeWakeStopPlatform(dirfd, agentDir, expected)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "not authoritative") {
		t.Fatalf("malformed stable-stop error = %v, want authoritative refusal", err)
	}
}
