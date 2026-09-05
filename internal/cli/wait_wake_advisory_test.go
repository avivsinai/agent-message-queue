package cli

import (
	"strings"
	"testing"
)

// Issue #717: a blocking wait started while a live injecting wake already
// notifies the terminal gets one advisory note naming drain as the remedy; a
// notify-only wake (the supervisor recipe) gets none.
func TestBlockingWaitNotesLiveInjectingWakeOnly(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mode     string
		wantNote bool
	}{
		{name: "inject-via wake", mode: wakeTargetInjectVia, wantNote: true},
		{name: "notify-only wake", mode: wakeInjectModeNone, wantNote: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			writeWakeLockForTest(t, root, "codex", wakeLock{
				PID:          4242,
				TTY:          "unknown",
				ProcessStart: "start-1",
				BootID:       "boot-1",
				Executable:   "/usr/bin/amq",
				Args:         []string{"/usr/bin/amq", "wake", "--root", root, "--me", "codex"},
				WakeMode:     tc.mode,
			})
			stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
				return wakeProcessInfo{
					PID: pid, Running: true, StartToken: "start-1", BootID: "boot-1", Executable: "/usr/bin/amq",
					Args: []string{"/usr/bin/amq", "wake", "--root", root, "--me", "codex"},
				}
			})
			out := captureWakeStderr(t, func() { noteLiveWakeBeforeBlockingWait("watch", root, "codex") })
			if got := strings.Contains(out, "already notifies this terminal"); got != tc.wantNote {
				t.Fatalf("note emitted = %v, want %v; stderr=%q", got, tc.wantNote, out)
			}
			if tc.wantNote && !strings.Contains(out, "amq drain --include-body") {
				t.Fatalf("note does not name the remedy: %q", out)
			}
		})
	}
}
