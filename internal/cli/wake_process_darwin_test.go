//go:build darwin

package cli

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestInspectWakeProcessPlatformTreatsDarwinZombieAsNotRunning(t *testing.T) {
	old := readDarwinKinfoProc
	readDarwinKinfoProc = func(name string, args ...int) (*unix.KinfoProc, error) {
		if name != "kern.proc.pid" || len(args) != 1 || args[0] != os.Getpid() {
			t.Fatalf("unexpected sysctl request: name=%q args=%v", name, args)
		}
		return &unix.KinfoProc{Proc: unix.ExternProc{P_stat: darwinProcessStateZombie}}, nil
	}
	t.Cleanup(func() { readDarwinKinfoProc = old })

	info := inspectWakeProcessPlatform(os.Getpid())
	if info.Running {
		t.Fatalf("zombie process reported running: %#v", info)
	}
}

func TestInspectWakeProcessPlatformPreservesAliveStateWhenKinfoFails(t *testing.T) {
	old := readDarwinKinfoProc
	readDarwinKinfoProc = func(name string, args ...int) (*unix.KinfoProc, error) {
		if name != "kern.proc.pid" || len(args) != 1 || args[0] != os.Getpid() {
			t.Fatalf("unexpected sysctl request: name=%q args=%v", name, args)
		}
		return nil, errors.New("sysctl kinfo failed")
	}
	t.Cleanup(func() { readDarwinKinfoProc = old })

	info := inspectWakeProcessPlatform(os.Getpid())
	if !info.Running || info.InspectError == nil {
		t.Fatalf("inspection = %#v; want running with inspection error", info)
	}
}

func TestInspectWakeProcessPlatformReportsControllingTerminalState(t *testing.T) {
	for _, tc := range []struct {
		name string
		tdev int32
		has  bool
	}{
		{name: "terminal gone", tdev: darwinNoControllingTerminal, has: false},
		{name: "terminal present", tdev: 1234, has: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := readDarwinKinfoProc
			readDarwinKinfoProc = func(string, ...int) (*unix.KinfoProc, error) {
				return &unix.KinfoProc{
					Proc:  unix.ExternProc{P_stat: 1},
					Eproc: unix.Eproc{Tdev: tc.tdev},
				}, nil
			}
			t.Cleanup(func() { readDarwinKinfoProc = old })

			info := inspectWakeProcessPlatform(os.Getpid())
			if !info.ControllingTerminalKnown || info.HasControllingTerminal != tc.has {
				t.Fatalf("terminal state = known:%v has:%v, want known:true has:%v",
					info.ControllingTerminalKnown, info.HasControllingTerminal, tc.has)
			}
		})
	}
}

func stubDarwinBootIdentityReaders(
	t *testing.T,
	session func() (string, error),
	bootTime func() (*unix.Timeval, error),
) {
	t.Helper()
	oldSession := readDarwinBootSessionUUID
	oldBootTime := readDarwinBootTime
	readDarwinBootSessionUUID = session
	readDarwinBootTime = bootTime
	t.Cleanup(func() {
		readDarwinBootSessionUUID = oldSession
		readDarwinBootTime = oldBootTime
	})
}

func TestDarwinBootIdentityPrefersBootSessionUUIDAndKeepsLegacyAlias(t *testing.T) {
	bootTime := unix.NsecToTimeval(1783327533407566000)
	stubDarwinBootIdentityReaders(t,
		func() (string, error) { return "  9C0682F4-901B-4243-8B5C-287FAFB9AD0E\n", nil },
		func() (*unix.Timeval, error) { return &bootTime, nil },
	)

	bootID, legacyBootID := darwinBootIdentity()
	if bootID != "9C0682F4-901B-4243-8B5C-287FAFB9AD0E" {
		t.Fatalf("boot id = %q", bootID)
	}
	if legacyBootID != "1783327533.407566000" {
		t.Fatalf("legacy boot id = %q", legacyBootID)
	}
}

func TestDarwinBootIdentityFallsBackToBootTime(t *testing.T) {
	bootTime := unix.NsecToTimeval(1783327533407566000)
	stubDarwinBootIdentityReaders(t,
		func() (string, error) { return "", errors.New("unsupported") },
		func() (*unix.Timeval, error) { return &bootTime, nil },
	)

	bootID, legacyBootID := darwinBootIdentity()
	if bootID != "1783327533.407566000" {
		t.Fatalf("fallback boot id = %q", bootID)
	}
	if legacyBootID != "" {
		t.Fatalf("fallback legacy boot id = %q, want empty", legacyBootID)
	}
}

func TestDarwinBootIdentityReturnsEmptyWhenBothSourcesUnavailable(t *testing.T) {
	stubDarwinBootIdentityReaders(t,
		func() (string, error) { return "\x00", nil },
		func() (*unix.Timeval, error) { return nil, errors.New("unsupported") },
	)

	bootID, legacyBootID := darwinBootIdentity()
	if bootID != "" || legacyBootID != "" {
		t.Fatalf("boot ids = %q, %q; want empty", bootID, legacyBootID)
	}
}
