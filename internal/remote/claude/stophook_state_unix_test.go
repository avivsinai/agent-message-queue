//go:build !windows

package claude

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// codex #869 r1: the doctor check read settings.json with an unbounded
// open, so a FIFO there hung doctor, and a marked hook under
// disableAllHooks read as installed.
func TestStopHookStateIsBoundedAndHonorsDisable(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(settingsPath(home), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if _, err := StopHookState(home); err == nil {
		t.Fatal("a FIFO settings file was read as settings")
	}
	if err := os.Remove(settingsPath(home)); err != nil {
		t.Fatal(err)
	}
	if err := InstallStopHook(home, "/usr/local/bin/amq-remote"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(settingsPath(home))
	disabled := append([]byte(`{"disableAllHooks":true,`), raw[1:]...)
	if err := os.WriteFile(settingsPath(home), disabled, 0o600); err != nil {
		t.Fatal(err)
	}
	if state, err := StopHookState(home); err != nil || state != StopHookDisabled {
		t.Fatalf("state = %q err = %v, want disabled", state, err)
	}
}
