package cli

import "testing"

func TestStillMatchesLiveWakeUncheckableBootIsStillPresent(t *testing.T) {
	insp := wakeLockInspection{PID: 4343, Lock: wakeLock{PID: 4343, ProcessStart: "start-1", BootID: "recorded-boot", Executable: "/opt/homebrew/bin/amq"}, Root: "/tmp/x", Agent: "codex"}
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: true, StartToken: "start-1", Executable: "/opt/homebrew/bin/amq", Args: []string{"/opt/homebrew/bin/amq", "wake", "--root", "/tmp/x", "--me", "codex"}}
	})
	if !wakeProcessStillMatches(insp) {
		t.Fatal("live wake with unknown boot identity must remain present")
	}
}

func TestStillMatchesDifferentUUIDBootIsGone(t *testing.T) {
	insp := wakeLockInspection{PID: 4343, Lock: wakeLock{PID: 4343, ProcessStart: "start-1", BootID: "9C0682F4-901B-4243-8B5C-287FAFB9AD0E", Executable: "/opt/homebrew/bin/amq"}, Root: "/tmp/x", Agent: "codex"}
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid, Running: true, StartToken: "start-1", BootID: "AAAAAAAA-901B-4243-8B5C-287FAFB9AD0E", Executable: "/opt/homebrew/bin/amq", Args: []string{"/opt/homebrew/bin/amq", "wake", "--root", "/tmp/x", "--me", "codex"}}
	})
	if wakeProcessStillMatches(insp) {
		t.Fatal("different UUID boot must be gone")
	}
}
