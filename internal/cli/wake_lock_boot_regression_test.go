package cli

import "testing"

func TestWakeIdentityStateIsUnknownWhenLiveWakeBootIsUnavailable(t *testing.T) {
	insp := wakeLockInspection{PID: 4343, Lock: wakeLock{PID: 4343, ProcessStart: "start-1", BootID: "recorded-boot", Executable: "/opt/homebrew/bin/amq"}, Root: "/tmp/x", Agent: "codex"}
	proc := wakeProcessInfo{PID: 4343, Running: true, StartToken: "start-1", Executable: "/opt/homebrew/bin/amq", Args: []string{"/opt/homebrew/bin/amq", "wake", "--root", "/tmp/x", "--me", "codex"}}
	stubInspectWakeProcess(t, func(int) wakeProcessInfo { return proc })
	if got := inspectWakeIdentity(insp); got != wakeIdentityUnknown {
		t.Fatalf("inspectWakeIdentity() = %v, want unknown", got)
	}
	// An unreadable boot id is not reported as a boot id mismatch (audit 2026-11-10).
	if _, reason := classifyWakeIdentity(insp, proc); reason == "boot id mismatch" {
		t.Fatalf("classifyWakeIdentity() reason = %q, want the unknown-boot reason", reason)
	}
}
