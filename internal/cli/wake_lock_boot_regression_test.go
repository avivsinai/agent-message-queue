package cli

import "testing"

func TestWakeIdentityStateIsSameForMatchingProcess(t *testing.T) {
	insp := wakeLockInspection{PID: 4343, Lock: wakeLock{PID: 4343, ProcessStart: "start-1", BootID: "recorded-boot"}, Root: "/tmp/x", Agent: "codex"}
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: true, StartToken: "start-1", BootID: "recorded-boot", Executable: "/opt/homebrew/bin/amq", Args: []string{"/opt/homebrew/bin/amq", "wake", "--root", "/tmp/x", "--me", "codex"}}
	})
	if got := inspectWakeIdentity(insp); got != wakeIdentitySame {
		t.Fatalf("inspectWakeIdentity() = %v, want same", got)
	}
}

func TestWakeIdentityStateIsUnknownWhenLiveWakeBootIsUnavailable(t *testing.T) {
	insp := wakeLockInspection{PID: 4343, Lock: wakeLock{PID: 4343, ProcessStart: "start-1", BootID: "recorded-boot", Executable: "/opt/homebrew/bin/amq"}, Root: "/tmp/x", Agent: "codex"}
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: true, StartToken: "start-1", Executable: "/opt/homebrew/bin/amq", Args: []string{"/opt/homebrew/bin/amq", "wake", "--root", "/tmp/x", "--me", "codex"}}
	})
	if got := inspectWakeIdentity(insp); got != wakeIdentityUnknown {
		t.Fatalf("inspectWakeIdentity() = %v, want unknown", got)
	}
}

func TestWakeIdentityStateIsGoneOrDifferentForDifferentUUIDBoot(t *testing.T) {
	insp := wakeLockInspection{PID: 4343, Lock: wakeLock{PID: 4343, ProcessStart: "start-1", BootID: "9C0682F4-901B-4243-8B5C-287FAFB9AD0E", Executable: "/opt/homebrew/bin/amq"}, Root: "/tmp/x", Agent: "codex"}
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: true, StartToken: "start-1", BootID: "AAAAAAAA-901B-4243-8B5C-287FAFB9AD0E", Executable: "/opt/homebrew/bin/amq", Args: []string{"/opt/homebrew/bin/amq", "wake", "--root", "/tmp/x", "--me", "codex"}}
	})
	if got := inspectWakeIdentity(insp); got != wakeIdentityGoneOrDifferent {
		t.Fatalf("inspectWakeIdentity() = %v, want gone or different", got)
	}
}
