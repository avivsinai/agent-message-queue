//go:build darwin

package cli

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const (
	darwinWakeOwnerDescendantHelperEnv  = "AMQ_TEST_DARWIN_WAKE_OWNER_DESCENDANT"
	darwinWakeOwnerDescendantProbeFDEnv = "AMQ_TEST_DARWIN_WAKE_OWNER_PROBE_FD"
)

func TestDarwinWakeOwnerChildWriterRemainsCloseOnExecAfterBind(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 30")
	cmd.Env = os.Environ()
	capability, err := configureAuthoritativeWakeChild(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		_ = capability.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
		_ = capability.Close()
	})
	if err := capability.Bind(cmd.Process); err != nil {
		t.Fatal(err)
	}

	// Bind validates the actual retained writer after the child has spawned.
	// A missing FD_CLOEXEC is a hard error because this pipe authorizes startup
	// rollback only; it must disappear when coop exec replaces the parent.
	if err := capability.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestDarwinWakeOwnerPrivateStopRequiresExplicitByte(t *testing.T) {
	t.Run("explicit rollback byte stops", func(t *testing.T) {
		readEnd, writeEnd, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		stop, cleanup := watchAuthoritativeWakePrivateStop(readEnd)
		defer cleanup()
		if _, err := writeEnd.Write([]byte{1}); err != nil {
			_ = writeEnd.Close()
			t.Fatal(err)
		}
		_ = writeEnd.Close()
		select {
		case <-stop:
		case <-time.After(2 * time.Second):
			t.Fatal("explicit startup rollback byte did not stop the wake child")
		}
	})

	t.Run("successful exec EOF does not stop", func(t *testing.T) {
		readEnd, writeEnd, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		stop, cleanup := watchAuthoritativeWakePrivateStop(readEnd)
		if err := writeEnd.Close(); err != nil {
			cleanup()
			t.Fatal(err)
		}
		cleanup()
		select {
		case <-stop:
			t.Fatal("writer EOF was misclassified as exact owner death")
		default:
		}
	})
}

func TestDarwinWakeOwnerPrivateStopCleanupInterruptsAndJoinsRead(t *testing.T) {
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writeEnd.Close() }()
	stop, cleanup := watchAuthoritativeWakePrivateStop(readEnd)

	finished := make(chan struct{})
	go func() {
		cleanup()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("private-stop cleanup did not interrupt and join the blocking read")
	}
	select {
	case <-stop:
		t.Fatal("cleanup was misclassified as explicit startup rollback")
	default:
	}
}

func TestDarwinWakeOwnerPrivateStopIsSealedFromDescendantExec(t *testing.T) {
	if os.Getenv(darwinWakeOwnerDescendantHelperEnv) != "" {
		if raw := os.Getenv(envWakePrivateStopFD); raw != "" {
			t.Fatalf("descendant inherited %s=%q", envWakePrivateStopFD, raw)
		}
		fd, err := strconv.Atoi(os.Getenv(darwinWakeOwnerDescendantProbeFDEnv))
		if err != nil {
			t.Fatalf("parse descendant probe fd: %v", err)
		}
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
			t.Fatalf("descendant probe fd %d = %v, want EBADF", fd, err)
		}
		return
	}

	var pipeFDs [2]int
	if err := unix.Pipe(pipeFDs[:]); err != nil {
		t.Fatal(err)
	}
	readFD, writeFD := pipeFDs[0], pipeFDs[1]
	defer func() { _ = unix.Close(writeFD) }()
	t.Setenv(envWakePrivateStopFD, strconv.Itoa(readFD))
	stop, cleanup, err := authoritativeWakePrivateStopFromEnv()
	if err != nil {
		_ = unix.Close(readFD)
		t.Fatal(err)
	}
	defer cleanup()
	if raw := os.Getenv(envWakePrivateStopFD); raw != "" {
		t.Fatalf("%s survived ingestion as %q", envWakePrivateStopFD, raw)
	}

	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(
		testBinary,
		"-test.run=^TestDarwinWakeOwnerPrivateStopIsSealedFromDescendantExec$",
	)
	cmd.Env = append(
		os.Environ(),
		darwinWakeOwnerDescendantHelperEnv+"=1",
		darwinWakeOwnerDescendantProbeFDEnv+"="+strconv.Itoa(readFD),
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("descendant capability probe: %v\n%s", err, output)
	}

	if _, err := unix.Write(writeFD, []byte{1}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stop:
	case <-time.After(2 * time.Second):
		t.Fatal("sealed private-stop descriptor did not receive rollback byte")
	}
}

func TestDarwinWakeOwnerPrivateStopInvalidEnvFailsClosed(t *testing.T) {
	for _, raw := range []string{"not-an-fd", "1073741824"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv(envWakePrivateStopFD, raw)
			stop, cleanup, err := authoritativeWakePrivateStopFromEnv()
			if err == nil {
				if cleanup != nil {
					cleanup()
				}
				t.Fatalf("%s=%q was accepted", envWakePrivateStopFD, raw)
			}
			if stop != nil || cleanup != nil {
				t.Fatalf(
					"invalid private-stop capability returned stop=%t cleanup=%t",
					stop != nil,
					cleanup != nil,
				)
			}
			if got := os.Getenv(envWakePrivateStopFD); got != "" {
				t.Fatalf("%s survived failed ingestion as %q", envWakePrivateStopFD, got)
			}
		})
	}
}

func TestDarwinWakeOwnerChildWriterStartsCloseOnExec(t *testing.T) {
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = readEnd.Close()
		_ = writeEnd.Close()
	}()
	if err := validateDarwinWakeOwnerStartupRollbackFD(writeEnd); err != nil {
		t.Fatal(err)
	}
	flags, err := unix.FcntlInt(writeEnd.Fd(), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unix.FcntlInt(writeEnd.Fd(), unix.F_SETFD, flags&^unix.FD_CLOEXEC); err != nil {
		t.Fatal(err)
	}
	if err := validateDarwinWakeOwnerStartupRollbackFD(writeEnd); err == nil {
		t.Fatal("startup rollback writer accepted without close-on-exec")
	}
}
