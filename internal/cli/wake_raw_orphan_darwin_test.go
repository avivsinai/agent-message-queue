//go:build darwin

package cli

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLiveRawOrphanState(t *testing.T) {
	i := wakeLockInspection{
		IdentityConfirmed: true,
		Process: wakeProcessInfo{
			Running:                  true,
			ControllingTerminalKnown: true,
			HasControllingTerminal:   false,
		},
		Lock: wakeLock{WakeMode: "raw"},
	}
	if !isLiveRawOrphan(i) {
		t.Fatal("expected live raw orphan")
	}
	i.Lock.OwnerSchema = wakeOwnerLockSchema
	i.Lock.Owner = &wakeOwner{
		PID:          42,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    7,
	}
	if isLiveRawOrphan(i) {
		t.Fatal("owner-bound wake is not a live raw orphan")
	}
	i.Lock.OwnerSchema = 0
	i.Lock.Owner = nil
	i.Process.Running = false
	if isLiveRawOrphan(i) {
		t.Fatal("dead process is not a live raw orphan")
	}
}

func TestLiveRawOrphanDoesNotDependOnTerminalEvidence(t *testing.T) {
	tests := []struct {
		name string
		tty  string
		proc wakeProcessInfo
	}{
		{
			name: "attached",
			tty:  "/dev/null",
			proc: wakeProcessInfo{
				ControllingTerminalKnown: true,
				HasControllingTerminal:   true,
			},
		},
		{
			name: "attached with unknown saved tty",
			tty:  "unknown",
			proc: wakeProcessInfo{
				ControllingTerminalKnown: true,
				HasControllingTerminal:   true,
			},
		},
		{name: "undeterminable", tty: "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.proc.Running = true
			i := wakeLockInspection{
				IdentityConfirmed: true,
				Process:           tt.proc,
				Lock:              wakeLock{WakeMode: "raw", TTY: tt.tty},
			}
			if !isLiveRawOrphan(i) {
				t.Fatal("live identity-confirmed ownerless raw wake must be a takeover candidate")
			}
		})
	}
}

func TestWakeLockNeedsReplacementDarwinUsesLiveTerminalDevice(t *testing.T) {
	const (
		wakePID = 66121
		tdev    = 268435464
	)
	oldKinfo := readDarwinKinfoProc
	readDarwinKinfoProc = func(name string, args ...int) (*unix.KinfoProc, error) {
		if name != "kern.proc.pid" || len(args) != 1 || args[0] != os.Getpid() {
			t.Fatalf("unexpected current-process lookup: name=%q args=%v", name, args)
		}
		return &unix.KinfoProc{
			Proc:  unix.ExternProc{P_stat: 1},
			Eproc: unix.Eproc{Tdev: tdev},
		}, nil
	}
	t.Cleanup(func() { readDarwinKinfoProc = oldKinfo })
	stubWakeProcessSID(t, func(pid int) (int, error) {
		if pid == wakePID {
			return 100, nil
		}
		if pid == 0 {
			return 200, nil
		}
		t.Fatalf("unexpected SID lookup for pid %d", pid)
		return 0, nil
	})

	inspection := wakeLockInspection{
		IdentityConfirmed: true,
		Lock: wakeLock{
			PID: wakePID,
			TTY: "unknown",
		},
		Process: wakeProcessInfo{
			PID:                       wakePID,
			Running:                   true,
			ControllingTerminalKnown:  true,
			HasControllingTerminal:    true,
			ControllingTerminalDevice: tdev,
		},
	}
	if !wakeLockNeedsReplacement(inspection) {
		t.Fatal("same terminal device in a different session must trigger automatic replacement")
	}
}

func TestDoctorReportsLiveRawOrphan(t *testing.T) {
	const pid = 66121
	root := secureTempDirForTest(t)
	writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          pid,
		TTY:          "unknown",
		ProcessStart: "recorded-start",
		BootID:       "recorded-boot",
		Executable:   "/opt/homebrew/bin/amq",
		Generation:   "live-raw-orphan",
	})
	stubInspectWakeProcess(t, func(gotPID int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:                      gotPID,
			Running:                  true,
			StartToken:               "recorded-start",
			BootID:                   "recorded-boot",
			Executable:               "/opt/homebrew/bin/amq",
			ControllingTerminalKnown: true,
			HasControllingTerminal:   false,
		}
	})

	locks := checkWakeLocks(root, []string{"codex"}, false)
	if len(locks) != 1 {
		t.Fatalf("wake locks = %#v", locks)
	}
	got := locks[0]
	if got.Status != "live-raw-orphan" {
		t.Fatalf("status = %q", got.Status)
	}
}
