package cli

import (
	"testing"
)

const (
	testMachineUUID  = "11111111-2222-3333-4444-555555555555"
	legacyBoottimeID = "1781686509.734623000"
)

func stubCurrentWakeMachineID(t *testing.T, id string) {
	t.Helper()
	old := currentWakeMachineID
	currentWakeMachineID = func() string { return id }
	t.Cleanup(func() { currentWakeMachineID = old })
}

func stubCurrentWakeBootID(t *testing.T, id string) {
	t.Helper()
	old := currentWakeBootID
	currentWakeBootID = func() string { return id }
	t.Cleanup(func() { currentWakeBootID = old })
}

// A dead wake from an old boot of this same machine must classify stale, not
// unverified, even when the network-derived hostname drifted after the lock
// was written. This is the resume-after-reboot scenario that used to force an
// interactive prompt on every coop exec.
func TestInspectWakeLockHostnameDriftOnSameMachineDeadPIDIsStale(t *testing.T) {
	root := secureTempDirForTest(t)
	writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          66121,
		TTY:          "ttys001",
		Hostname:     "definitely-not-this-host",
		MachineID:    testMachineUUID,
		ProcessStart: "1783283179.960200000",
		BootID:       legacyBoottimeID,
		Executable:   "amq",
	})
	stubCurrentWakeMachineID(t, testMachineUUID)
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: false}
	})

	inspection := inspectWakeLock(root, "codex")
	if inspection.Status != wakeLockStale {
		t.Fatalf("inspection status = %q (reason %q), want stale", inspection.Status, inspection.Reason)
	}
}
