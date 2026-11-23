//go:build darwin || linux

package cli

import (
	"strings"
	"testing"
)

// With --no-wake (or beside another owner's wake) the launch owns no wake,
// and coop exec still names the seat's delivery: an attended seat prints
// the attended line, because the hooks treat this seat as attended all the
// same. Deleting the else-if in coop exec's startup block removes the line
// and fails this test.
func TestCoopExecNoWakeNamesAttendedDelivery(t *testing.T) {
	// The hook installer writes the shared Claude config; give this test its
	// own HOME so later tests see no hook.
	t.Setenv("HOME", t.TempDir())
	base := initCoopProjectForTest(t, "alice")
	clearCoopSessionPinForTest(t)
	// exec provisions and reads the default session root, not the base.
	setAttendedForTest(t, base+"/collab", "alice")
	// The dialog hook goes to a private home: one installed in the package's
	// shared test home made the plain-doorbell test see a dialog guard and
	// hold its doorbell (full-suite run after the attended-seat merge).
	t.Setenv("HOME", t.TempDir())
	fakeClaudeOnPath(t, "2.1.310")
	if _, err := captureStdout(t, func() error {
		return runWake([]string{"install-dialog-hook"})
	}); err != nil {
		t.Fatalf("install-dialog-hook: %v", err)
	}
	_, stderr, err := captureCoopExecStderr(t, []string{"--no-wake", "--me", "alice", "claude"})
	if err != nil {
		t.Fatalf("coop exec: %v", err)
	}
	if !strings.Contains(stderr, "delivery: attended (no doorbell is typed; mail shows as a notice)\n") {
		t.Fatalf("no-wake attended stderr = %q, want the attended delivery line", stderr)
	}
}
