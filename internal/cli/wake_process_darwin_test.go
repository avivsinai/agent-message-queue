//go:build darwin

package cli

import (
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
