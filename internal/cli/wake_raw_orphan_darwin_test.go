//go:build darwin

package cli

import (
	"testing"
)

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

	locks := runOpsChecks(root, "test", false).WakeLocks
	if len(locks) != 1 {
		t.Fatalf("wake locks = %#v", locks)
	}
	got := locks[0]
	if got.Status != "live-raw-orphan" {
		t.Fatalf("status = %q", got.Status)
	}
}
