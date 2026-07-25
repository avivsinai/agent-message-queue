//go:build darwin

package cli

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestDarwinWakeRepairChildControlStopsOnExplicitStop(t *testing.T) {
	assertDarwinWakeRepairChildControl(t, wakeRepairChildControlStop, true)
}

func TestDarwinWakeRepairChildControlStopsOnEOFBeforeDetach(t *testing.T) {
	assertDarwinWakeRepairChildControl(t, "", true)
}

func TestDarwinWakeRepairChildControlDetachDisarmsEOF(t *testing.T) {
	assertDarwinWakeRepairChildControl(t, wakeRepairChildControlDetach, false)
}

func TestDarwinWakeRepairChildCleanupInterruptsBlockingControlRead(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	childFD, err := unix.Dup(int(reader.Fd()))
	if err != nil {
		_ = reader.Close()
		t.Fatalf("duplicate blocking child control fd: %v", err)
	}
	if err := reader.Close(); err != nil {
		_ = unix.Close(childFD)
		t.Fatalf("close original child control reader: %v", err)
	}
	if err := unix.SetNonblock(childFD, false); err != nil {
		_ = unix.Close(childFD)
		t.Fatalf("force inherited child control fd blocking: %v", err)
	}
	flags, err := unix.FcntlInt(uintptr(childFD), unix.F_GETFL, 0)
	if err != nil {
		_ = unix.Close(childFD)
		t.Fatalf("inspect blocking child control fd: %v", err)
	}
	if flags&unix.O_NONBLOCK != 0 {
		_ = unix.Close(childFD)
		t.Fatal("child control regression fixture is unexpectedly nonblocking")
	}
	t.Setenv(envWakeRepairChildControlFD, strconv.Itoa(childFD))

	stop, cleanup, err := wakeRepairChildStopFromEnv()
	if err != nil {
		_ = unix.Close(childFD)
		t.Fatalf("start child control watcher: %v", err)
	}
	flags, err = unix.FcntlInt(uintptr(childFD), unix.F_GETFL, 0)
	if err != nil {
		cleanup()
		t.Fatalf("inspect initialized child control fd: %v", err)
	}
	if flags&unix.O_NONBLOCK == 0 {
		_ = writer.Close()
		cleanup()
		t.Fatal("inherited child control fd remained blocking")
	}
	fdFlags, err := unix.FcntlInt(uintptr(childFD), unix.F_GETFD, 0)
	if err != nil {
		_ = writer.Close()
		cleanup()
		t.Fatalf("inspect initialized child control descriptor flags: %v", err)
	}
	if fdFlags&unix.FD_CLOEXEC == 0 {
		_ = writer.Close()
		cleanup()
		t.Fatal("inherited child control fd is not close-on-exec")
	}
	assertWakeRepairDescriptorsClosedInInjector(t, []int{childFD})
	select {
	case <-stop:
		cleanup()
		t.Fatal("absent child control byte stopped the child")
	case <-time.After(50 * time.Millisecond):
	}
	finished := make(chan struct{})
	go func() {
		cleanup()
		close(finished)
	}()

	select {
	case <-finished:
	case <-time.After(250 * time.Millisecond):
		_ = writer.Close()
		<-finished
		t.Fatal("child control cleanup did not interrupt its blocked read")
	}
	select {
	case <-stop:
	default:
		t.Fatal("interrupted pre-admission child control did not fail closed")
	}
}

func assertDarwinWakeRepairChildControl(t *testing.T, command string, wantStop bool) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stop, finished := watchWakeRepairDarwinChildControl(reader)
	if command != "" {
		if _, err := writer.Write([]byte(command + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("control watcher did not finish")
	}
	select {
	case <-stop:
		if !wantStop {
			t.Fatal("detach unexpectedly stopped admitted child")
		}
	default:
		if wantStop {
			t.Fatal("stop was not signaled")
		}
	}
}

func TestDarwinWakeRepairChildCapabilityEmitsStopAndDetach(t *testing.T) {
	for _, test := range []struct {
		name string
		act  func(*wakeRepairChildCapability) error
		want string
	}{
		{name: "stop", act: func(c *wakeRepairChildCapability) error { return c.Stop() }, want: wakeRepairChildControlStop},
		{name: "detach", act: func(c *wakeRepairChildCapability) error { return c.Detach() }, want: wakeRepairChildControlDetach},
	} {
		t.Run(test.name, func(t *testing.T) {
			oldKill := killWakeRepairDarwinChild
			killWakeRepairDarwinChild = func(*os.Process) error { return nil }
			t.Cleanup(func() { killWakeRepairDarwinChild = oldKill })

			cmd := exec.Command("true")
			capability, err := prepareWakeRepairChildCapabilityPlatform(cmd)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = capability.Close() }()
			if len(cmd.ExtraFiles) != 1 {
				t.Fatalf("extra files = %d, want 1", len(cmd.ExtraFiles))
			}
			childFD, err := unix.Dup(int(cmd.ExtraFiles[0].Fd()))
			if err != nil {
				t.Fatalf("duplicate child control fd: %v", err)
			}
			childReader := os.NewFile(uintptr(childFD), "test-child-control")
			defer func() { _ = childReader.Close() }()
			if err := capability.Bind(&os.Process{Pid: 4242}); err != nil {
				t.Fatal(err)
			}
			if err := test.act(capability); err != nil {
				t.Fatal(err)
			}
			var line [16]byte
			n, err := childReader.Read(line[:])
			if err != nil {
				t.Fatal(err)
			}
			if got := string(line[:n]); got != test.want+"\n" {
				t.Fatalf("control = %q, want %q", got, test.want+"\\n")
			}
		})
	}
}

func TestDarwinWakeRepairChildCapabilityFullDetachWriteIsIrreversibleAcrossCloseFailure(t *testing.T) {
	cmd := exec.Command("true")
	capability, err := prepareWakeRepairChildCapabilityPlatform(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = capability.Close() }()
	childFD, err := unix.Dup(int(cmd.ExtraFiles[0].Fd()))
	if err != nil {
		t.Fatalf("duplicate child control fd: %v", err)
	}
	childReader := os.NewFile(uintptr(childFD), "test-child-control")
	defer func() { _ = childReader.Close() }()

	oldClose := closeWakeRepairDarwinChildControl
	oldKill := killWakeRepairDarwinChild
	closeFailure := errors.New("injected child control close failure")
	closeCalls := 0
	closeWakeRepairDarwinChildControl = func(file *os.File) error {
		closeCalls++
		if closeCalls == 1 {
			return closeFailure
		}
		return file.Close()
	}
	var killed *os.Process
	killWakeRepairDarwinChild = func(process *os.Process) error {
		killed = process
		return nil
	}
	t.Cleanup(func() {
		closeWakeRepairDarwinChildControl = oldClose
		killWakeRepairDarwinChild = oldKill
	})

	child := &os.Process{Pid: 4242}
	if err := capability.Bind(child); err != nil {
		t.Fatal(err)
	}
	if err := capability.Detach(); err != nil {
		t.Fatalf("detach after complete write and close failure: %v", err)
	}
	if err := capability.Stop(); err == nil {
		t.Fatal("Stop unexpectedly retained authority after complete DETACH write")
	}
	if killed != nil {
		t.Fatalf("killed process = %p, want no kill after complete DETACH write", killed)
	}
}

func TestDarwinWakeRepairChildCapabilityPartialDetachWriteRetainsExactStopAuthority(t *testing.T) {
	cmd := exec.Command("true")
	capability, err := prepareWakeRepairChildCapabilityPlatform(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = capability.Close() }()

	oldWrite := writeWakeRepairDarwinChildControl
	oldKill := killWakeRepairDarwinChild
	writeFailure := errors.New("injected partial DETACH write failure")
	writeCalls := 0
	writeWakeRepairDarwinChildControl = func(_ io.Writer, command string) error {
		writeCalls++
		if writeCalls == 1 {
			if command != wakeRepairChildControlDetach {
				t.Fatalf("first control command = %q, want DETACH", command)
			}
			return writeFailure
		}
		if command != wakeRepairChildControlStop {
			t.Fatalf("fallback control command = %q, want STOP", command)
		}
		return nil
	}
	var killed *os.Process
	killWakeRepairDarwinChild = func(process *os.Process) error {
		killed = process
		return nil
	}
	t.Cleanup(func() {
		writeWakeRepairDarwinChildControl = oldWrite
		killWakeRepairDarwinChild = oldKill
	})

	child := &os.Process{Pid: 4242}
	if err := capability.Bind(child); err != nil {
		t.Fatal(err)
	}
	if err := capability.Detach(); !errors.Is(err, writeFailure) {
		t.Fatalf("detach error = %v, want %v", err, writeFailure)
	}
	if err := capability.Stop(); err != nil {
		t.Fatalf("fallback exact-child stop: %v", err)
	}
	if killed != child {
		t.Fatalf("killed process = %p, want exact bound child %p", killed, child)
	}
	if writeCalls != 2 {
		t.Fatalf("control writes = %d, want DETACH failure then STOP", writeCalls)
	}
}

func TestDarwinWakeRepairChildCapabilityStopsChildBlockedBeforeControlRead(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "30")
	capability, err := prepareWakeRepairChildCapabilityPlatform(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = capability.Close() }()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := capability.Bind(cmd.Process); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	if err := capability.Stop(); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("stop child blocked before control read: %v", err)
	}

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err := <-waited:
		if err == nil {
			t.Fatal("blocked child exited successfully after exact stop; want signal termination")
		}
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("exact pre-admission child remained alive after stop")
	}
}

func TestDarwinWakeRepairChildCapabilityStopAfterWaitCannotSignalReusedPID(t *testing.T) {
	cmd := exec.Command("/usr/bin/true")
	capability, err := prepareWakeRepairChildCapabilityPlatform(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = capability.Close() }()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := capability.Bind(cmd.Process); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait direct child: %v", err)
	}

	// os.Process synchronizes Wait with Signal and returns ErrProcessDone
	// without issuing a PID-based signal after the direct child is reaped.
	if err := cmd.Process.Kill(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("kill reaped direct child = %v, want os.ErrProcessDone", err)
	}
	if err := capability.Stop(); err != nil {
		t.Fatalf("capability stop after direct-child Wait: %v", err)
	}
}
