//go:build linux

package cli

import (
	"errors"
	"os"
	"os/exec"
	"strings"
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
	var events []string
	oldOpen := linuxPidfdOpen
	oldPoll := linuxPidfdPoll
	oldClose := linuxPidfdClose
	linuxPidfdOpen = func(pid, flags int) (int, error) {
		events = append(events, "open")
		if pid != owner.PID || flags != 0 {
			t.Fatalf("pidfd_open(%d,%d)", pid, flags)
		}
		return 77, nil
	}
	linuxPidfdPoll = func(fd int, _ time.Duration) (bool, error) {
		events = append(events, "poll")
		if fd != 77 {
			t.Fatalf("poll fd = %d", fd)
		}
		return false, nil
	}
	linuxPidfdClose = func(fd int) error {
		events = append(events, "close")
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
	if inspections != 1 || polls != 2 {
		t.Fatalf("mid-snapshot exit inspected=%d polled=%d, want 1 and 2", inspections, polls)
	}
}

func TestLinuxUnsupportedPidfdStillCapturesCurrentOwnerForOwnerlessFallback(t *testing.T) {
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
		t.Fatalf("child capability error = %v, want ownerless-fallback signal", err)
	}
}

func TestLinuxFreshUnsupportedOwnerPidfdFallsBackWithoutPublishingOwnerMarkers(t *testing.T) {
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

	var cleanup func()
	var acquireErr error
	stderr := captureWakeStderr(t, func() {
		cleanup, acquireErr = acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
			target:   &target,
			wakeMode: wakeTargetInjectVia,
		})
	})
	if acquireErr != nil {
		t.Fatalf("ownerless fallback acquisition: %v", acquireErr)
	}
	defer cleanup()
	if !strings.Contains(stderr, "ownerless") {
		t.Fatalf("fallback warning = %q", stderr)
	}
	inspection := inspectWakeLock(root, "codex")
	if inspection.fileInfo.Mode().Perm() != 0o600 ||
		inspection.Lock.OwnerSchema != 0 ||
		inspection.Lock.Owner != nil ||
		inspection.Lock.WakeMode != wakeTargetInjectVia {
		t.Fatalf("fallback lock published owner markers: %#v", inspection)
	}
	persisted, exists, err := readWakeTarget(root, "codex")
	if err != nil || !exists {
		t.Fatalf("fallback target = %#v exists=%v err=%v", persisted, exists, err)
	}
	if persisted.Owner != nil {
		t.Fatalf("fallback target retained owner: %#v", persisted.Owner)
	}
	if target.Owner != nil {
		t.Fatalf("caller target retained owner-health semantics after fallback: %#v", target.Owner)
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
