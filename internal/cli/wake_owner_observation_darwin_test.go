//go:build darwin

package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestDarwinWakeOwnerObservationSignalsExactOwnerExitWithLiveDescendant(t *testing.T) {
	for _, exitKind := range []string{"normal", "crash"} {
		t.Run(exitKind, func(t *testing.T) {
			cmd, owner, descendantPID, release := startDarwinWakeObservationOwner(t)
			observation, err := observeAuthoritativeWakeOwnerPlatform(owner)
			if err != nil {
				t.Fatalf("observe exact owner: %v", err)
			}
			defer func() {
				if err := observation.Close(); err != nil {
					t.Errorf("close owner observation: %v", err)
				}
			}()
			if observation.State != wakeOwnerSame {
				t.Fatalf("owner observation = %#v, want live exact owner", observation)
			}
			if observation.Done() == nil {
				t.Fatal("live owner observation has no death signal")
			}

			if exitKind == "normal" {
				if err := os.WriteFile(release, []byte("exit\n"), 0o600); err != nil {
					t.Fatalf("release owner: %v", err)
				}
			} else if err := cmd.Process.Kill(); err != nil {
				t.Fatalf("crash owner: %v", err)
			}
			waitErr := cmd.Wait()
			cmd.Process = nil
			if exitKind == "normal" && waitErr != nil {
				t.Fatalf("wait for normal owner exit: %v", waitErr)
			}
			if exitKind == "crash" && waitErr == nil {
				t.Fatal("crashed owner exited successfully")
			}

			descendant, err := os.FindProcess(descendantPID)
			if err != nil {
				t.Fatalf("find descendant: %v", err)
			}
			if err := descendant.Signal(syscall.Signal(0)); err != nil {
				t.Fatalf("descendant did not outlive exact owner: %v", err)
			}
			select {
			case <-observation.Done():
			case <-time.After(3 * time.Second):
				t.Fatal("exact owner exit was prolonged by a live descendant")
			}
		})
	}
}

func TestDarwinWakeOwnerObservationDeadHasClosedDone(t *testing.T) {
	cmd, owner, _, _ := startDarwinWakeObservationOwner(t)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill owner: %v", err)
	}
	_ = cmd.Wait()
	cmd.Process = nil

	observation, err := observeAuthoritativeWakeOwnerPlatform(owner)
	if err != nil {
		t.Fatalf("observe dead owner: %v", err)
	}
	defer func() {
		if err := observation.Close(); err != nil {
			t.Errorf("close dead observation: %v", err)
		}
	}()
	if observation.State != wakeOwnerDead {
		t.Fatalf("dead owner observation = %#v", observation)
	}
	if observation.Done() == nil {
		t.Fatal("dead owner observation has nil Done")
	}
	select {
	case <-observation.Done():
	default:
		t.Fatal("dead owner observation Done is not already closed")
	}
}

func TestDarwinWakeOwnerObservationCloseCancelsAndJoins(t *testing.T) {
	cmd, owner, _, _ := startDarwinWakeObservationOwner(t)
	observation, err := observeAuthoritativeWakeOwnerPlatform(owner)
	if err != nil {
		t.Fatalf("observe exact owner: %v", err)
	}
	if observation.State != wakeOwnerSame || observation.Done() == nil {
		t.Fatalf("live owner observation = %#v", observation)
	}

	const callers = 8
	var wait sync.WaitGroup
	errs := make(chan error, callers)
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
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not cancel and join Darwin owner monitor")
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("observation Close terminated the owner: %v", err)
	}
}

func TestDarwinWakeOwnerObservationCloseRetriesInterruptedCancellation(t *testing.T) {
	_, owner, _, _ := startDarwinWakeObservationOwner(t)
	observation, err := observeAuthoritativeWakeOwnerPlatform(owner)
	if err != nil {
		t.Fatalf("observe exact owner: %v", err)
	}
	if observation.State != wakeOwnerSame || observation.Done() == nil {
		t.Fatalf("live owner observation = %#v", observation)
	}

	oldWrite := writeDarwinWakeOwnerObservationCancel
	attempts := 0
	writeDarwinWakeOwnerObservationCancel = func(fd int, data []byte) (int, error) {
		attempts++
		if attempts == 1 {
			return 0, syscall.EINTR
		}
		return unix.Write(fd, data)
	}
	t.Cleanup(func() {
		writeDarwinWakeOwnerObservationCancel = oldWrite
	})

	if err := observation.Close(); err != nil {
		t.Fatalf("close after interrupted cancellation: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("cancellation writes = %d, want interrupted attempt plus retry", attempts)
	}
	select {
	case <-observation.Done():
	default:
		t.Fatal("interrupted cancellation retry did not join the monitor")
	}
}

func TestDarwinWakeOwnerObservationUnknownHasNilDone(t *testing.T) {
	_, owner, _, _ := startDarwinWakeObservationOwner(t)
	sidErr := errors.New("session identity unavailable")
	stubWakeProcessSID(t, func(int) (int, error) {
		return 0, sidErr
	})

	observation, err := observeAuthoritativeWakeOwnerPlatform(owner)
	if err == nil || !strings.Contains(err.Error(), sidErr.Error()) {
		t.Fatalf("unknown owner observation error = %v", err)
	}
	if observation.State != wakeOwnerUnknown {
		t.Fatalf("unknown owner observation = %#v", observation)
	}
	if observation.Done() != nil {
		t.Fatal("unknown owner observation published a death signal")
	}
	if err := observation.Close(); err != nil {
		t.Fatalf("close unknown observation: %v", err)
	}
}

func TestDarwinWakeOwnerObservationDescriptorsAreCloseOnExec(t *testing.T) {
	capability, err := openDarwinWakeOwnerObservationCapability(os.Getpid())
	if err != nil {
		t.Fatalf("open owner observation capability: %v", err)
	}
	defer func() {
		if err := capability.close(); err != nil {
			t.Errorf("close owner observation capability: %v", err)
		}
	}()
	for _, descriptor := range []struct {
		fd    int
		label string
	}{
		{fd: capability.kqueueFD, label: "kqueue"},
		{fd: capability.cancelReadFD, label: "cancel read"},
		{fd: capability.cancelWriteFD, label: "cancel write"},
	} {
		flags, err := unix.FcntlInt(uintptr(descriptor.fd), unix.F_GETFD, 0)
		if err != nil {
			t.Fatalf("inspect %s flags: %v", descriptor.label, err)
		}
		if flags&unix.FD_CLOEXEC == 0 {
			t.Fatalf("%s descriptor %d is not close-on-exec", descriptor.label, descriptor.fd)
		}
	}
}

func TestDarwinWakeOwnerEventClassificationPrioritizesExitThenError(t *testing.T) {
	const (
		pid      = 4242
		cancelFD = 17
	)
	cancel := unix.Kevent_t{
		Ident:  cancelFD,
		Filter: unix.EVFILT_READ,
	}
	failed := unix.Kevent_t{
		Flags: unix.EV_ERROR,
		Data:  int64(syscall.EBADF),
	}
	exited := unix.Kevent_t{
		Ident:  pid,
		Filter: unix.EVFILT_PROC,
		Fflags: unix.NOTE_EXIT,
	}

	gotExit, gotErr, gotCancel := classifyDarwinWakeOwnerObservationEvents(
		[]unix.Kevent_t{cancel, failed, exited},
		pid,
		cancelFD,
	)
	if !gotExit || gotErr != nil || gotCancel {
		t.Fatalf("exit priority = (%v, %v, %v)", gotExit, gotErr, gotCancel)
	}
	gotExit, gotErr, gotCancel = classifyDarwinWakeOwnerObservationEvents(
		[]unix.Kevent_t{cancel, failed},
		pid,
		cancelFD,
	)
	if gotExit || !errors.Is(gotErr, syscall.EBADF) || gotCancel {
		t.Fatalf("error priority = (%v, %v, %v)", gotExit, gotErr, gotCancel)
	}
}

func startDarwinWakeObservationOwner(
	t *testing.T,
) (*exec.Cmd, wakeOwner, int, string) {
	t.Helper()
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	release := filepath.Join(dir, "release")
	descendantPath := filepath.Join(dir, "descendant.pid")
	script := `
sleep 30 &
printf '%s\n' "$!" > "$1"
: > "$2"
while [ ! -e "$3" ]; do sleep 0.01; done
exit 0
`
	cmd := exec.Command("/bin/sh", "-c", script, "sh", descendantPath, ready, release)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start owner: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
		if data, err := os.ReadFile(descendantPath); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				if descendant, err := os.FindProcess(pid); err == nil {
					_ = descendant.Kill()
				}
			}
		}
	})
	waitForDarwinWakeObservationPath(t, ready)
	owner := authoritativeOwnerForDarwinWakeObservationTest(t, cmd.Process.Pid)
	data, err := os.ReadFile(descendantPath)
	if err != nil {
		t.Fatalf("read descendant pid: %v", err)
	}
	descendantPID, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || descendantPID <= 0 {
		t.Fatalf("parse descendant pid %q: %v", data, err)
	}
	return cmd, owner, descendantPID, release
}

func authoritativeOwnerForDarwinWakeObservationTest(t *testing.T, pid int) wakeOwner {
	t.Helper()
	process := inspectWakeProcess(pid)
	sessionID, sessionErr := getWakeProcessSID(pid)
	owner := wakeOwner{
		PID:          pid,
		ProcessStart: process.StartToken,
		BootID:       process.BootID,
		SessionID:    sessionID,
	}
	if !process.Running || sessionErr != nil {
		t.Fatalf("capture owner pid %d: process=%#v sessionErr=%v", pid, process, sessionErr)
	}
	if err := validateAuthoritativeWakeOwner(owner); err != nil {
		t.Fatalf("validate owner pid %d: %v", pid, err)
	}
	return owner
}

func waitForDarwinWakeObservationPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat %s: %v", path, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
