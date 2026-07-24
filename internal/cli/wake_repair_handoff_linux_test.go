//go:build linux

package cli

import (
	"errors"
	"os"
	"os/exec"
	"testing"
)

func TestLinuxWakeRepairChildCapabilityRetainsAtomicPidfd(t *testing.T) {
	cmd := exec.Command("true")
	capability, err := prepareWakeRepairChildCapabilityPlatform(cmd)
	if err != nil {
		t.Fatalf("prepare capability: %v", err)
	}
	defer func() { _ = capability.Close() }()
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.PidFD == nil {
		t.Fatal("repair child did not request pidfd at process creation")
	}
	*cmd.SysProcAttr.PidFD = 91

	oldStop := stopWakeRepairChildPidfd
	oldClose := closeWakeRepairChildPidfd
	var stopped, closed int
	stopWakeRepairChildPidfd = func(fd int) error {
		stopped = fd
		return nil
	}
	closeWakeRepairChildPidfd = func(fd int) error {
		closed = fd
		return nil
	}
	t.Cleanup(func() {
		stopWakeRepairChildPidfd = oldStop
		closeWakeRepairChildPidfd = oldClose
	})

	if err := capability.Bind(&os.Process{Pid: 4242}); err != nil {
		t.Fatalf("bind capability: %v", err)
	}
	if err := capability.Stop(); err != nil {
		t.Fatalf("stop capability: %v", err)
	}
	if stopped != 91 {
		t.Fatalf("stopped pidfd = %d, want 91", stopped)
	}
	if err := capability.Close(); err != nil {
		t.Fatalf("close capability: %v", err)
	}
	if closed != 91 {
		t.Fatalf("closed pidfd = %d, want 91", closed)
	}
}

func TestLinuxWakeRepairChildCapabilityDetachClosesWithoutStopping(t *testing.T) {
	cmd := exec.Command("true")
	capability, err := prepareWakeRepairChildCapabilityPlatform(cmd)
	if err != nil {
		t.Fatal(err)
	}
	*cmd.SysProcAttr.PidFD = 92

	oldStop := stopWakeRepairChildPidfd
	oldClose := closeWakeRepairChildPidfd
	stopWakeRepairChildPidfd = func(int) error {
		t.Fatal("detach must not stop admitted child")
		return nil
	}
	var closed int
	closeWakeRepairChildPidfd = func(fd int) error {
		closed = fd
		return nil
	}
	t.Cleanup(func() {
		stopWakeRepairChildPidfd = oldStop
		closeWakeRepairChildPidfd = oldClose
	})

	if err := capability.Bind(&os.Process{Pid: 4242}); err != nil {
		t.Fatal(err)
	}
	if err := capability.Detach(); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if closed != 92 {
		t.Fatalf("closed pidfd = %d, want 92", closed)
	}
}

func TestLinuxWakeRepairChildCapabilityDetachCloseFailureNeverReusesReleasedPidfd(t *testing.T) {
	cmd := exec.Command("true")
	capability, err := prepareWakeRepairChildCapabilityPlatform(cmd)
	if err != nil {
		t.Fatal(err)
	}
	*cmd.SysProcAttr.PidFD = 93

	oldStop := stopWakeRepairChildPidfd
	oldClose := closeWakeRepairChildPidfd
	closeFailure := errors.New("injected pidfd close failure")
	var stopped, closeCalls int
	released := false
	stopWakeRepairChildPidfd = func(fd int) error {
		if released && fd == 93 {
			t.Fatal("Stop targeted the released and potentially reused pidfd")
		}
		stopped = fd
		return nil
	}
	closeWakeRepairChildPidfd = func(fd int) error {
		if fd != 93 {
			t.Fatalf("closed pidfd = %d, want 93", fd)
		}
		closeCalls++
		released = true
		return closeFailure
	}
	t.Cleanup(func() {
		stopWakeRepairChildPidfd = oldStop
		closeWakeRepairChildPidfd = oldClose
	})

	if err := capability.Bind(&os.Process{Pid: 4242}); err != nil {
		t.Fatal(err)
	}
	if err := capability.Detach(); err != nil {
		t.Fatalf("detach after Linux close failure: %v", err)
	}
	if err := capability.Stop(); err == nil {
		t.Fatal("Stop unexpectedly retained authority after attempted pidfd close")
	}
	if stopped != 0 {
		t.Fatalf("stopped pidfd = %d, want no signal through released fd", stopped)
	}
	if err := capability.Close(); err != nil {
		t.Fatalf("close detached capability: %v", err)
	}
	if closeCalls != 1 {
		t.Fatalf("pidfd close calls = %d, want exactly one irrevocable attempt", closeCalls)
	}
}
