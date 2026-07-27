//go:build darwin

package cli

import "testing"

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
