package cli

import "testing"

func TestWakeIdentityStateIsUnknownWhenLiveWakeBootIsUnavailable(t *testing.T) {
	insp := wakeLockInspection{PID: 4343, Lock: wakeLock{PID: 4343, ProcessStart: "start-1", BootID: "recorded-boot", Executable: "/opt/homebrew/bin/amq"}, Root: "/tmp/x", Agent: "codex"}
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: true, StartToken: "start-1", Executable: "/opt/homebrew/bin/amq", Args: []string{"/opt/homebrew/bin/amq", "wake", "--root", "/tmp/x", "--me", "codex"}}
	})
	if got := inspectWakeIdentity(insp); got != wakeIdentityUnknown {
		t.Fatalf("inspectWakeIdentity() = %v, want unknown", got)
	}
}
