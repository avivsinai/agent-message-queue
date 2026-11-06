package claude

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Regression: on 2026-11-05 the approval hook left a settings.json because
// another writer replaced the file with its own copy while an edit was in
// flight. An install that another writer finishes first keeps both
// entries: the writer reads the file again before its rename and applies
// its edit to what it finds.
func TestConcurrentInstallsKeepBothEntries(t *testing.T) {
	home := t.TempDir()
	path := SettingsPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"model":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home, "bin", "amq-remote")
	approval := replaceEdit(permissionSpec, permissionHookEntry(home, bin, DefaultPermissionWait))
	other := true
	_, err := editSettings(path, permissionSpec.seed(), func(raw []byte) ([]byte, error) {
		if other {
			// The other install renames its file between this read and
			// this rename.
			other = false
			theirs, err := replaceEdit(stopHook, stopHookEntry(home, bin))(raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, theirs, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return approval(raw)
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, state := range map[string]func(string) (string, error){"approval": PermissionHookState, "stop": StopHookStateAt} {
		if got, err := state(path); err != nil || got != StopHookPresent {
			t.Errorf("%s hook = %q, %v; want both installs kept", name, got, err)
		}
	}
}

// The machine agent restores an approval hook another writer removed only
// while the owner's opt-in for that file exists; uninstall clears it.
func TestRestoreHookFollowsTheOptIn(t *testing.T) {
	home := t.TempDir()
	path := SettingsPath(home)
	bin := filepath.Join(home, "amq-remote")
	if err := os.WriteFile(bin, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := OptInPermissionHook(home, path, bin, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"model":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if restored, err := RestoreHook(home, path, HookApproval, bin); err != nil || !restored {
		t.Fatalf("restore = %v, %v; want restored", restored, err)
	}
	if form, _ := PermissionHookForm(home, path); form != HookFormCurrent {
		t.Fatalf("restored hook form = %q", form)
	}
	if err := OptOutHook(path, HookApproval); err != nil {
		t.Fatal(err)
	}
	if restored, err := RestoreHook(home, path, HookApproval, bin); err != nil || restored {
		t.Fatalf("restore after uninstall = %v, %v; want nothing", restored, err)
	}
}
