//go:build darwin

package cli

import (
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
