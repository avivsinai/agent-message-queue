package claude

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	approval := replaceEdit(permissionSpec, permissionHookEntry(home, bin))
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
		if got, err := state(path); err != nil || got != HookPresent {
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
	if _, err := OptInPermissionHook(home, path, bin); err != nil {
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

// Review of the hook installers: RestoreHook recreated a settings file the
// owner had deleted.
func TestRestoreHookLeavesADeletedSettingsFileDeleted(t *testing.T) {
	home := t.TempDir()
	path := SettingsPath(home)
	bin := filepath.Join(home, "amq-remote")
	if err := os.WriteFile(bin, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := OptInPermissionHook(home, path, bin); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if restored, err := RestoreHook(home, path, HookApproval, bin); err != nil || restored {
		t.Fatalf("restore = %v, %v; want nothing", restored, err)
	}
	// Review of the hook upgrade: an upgrade raced with the owner's delete
	// and wrote the file back from a seed.
	if upgraded, err := UpgradeHook(home, path, HookApproval, bin); err != nil || upgraded {
		t.Fatalf("upgrade = %v, %v; want nothing", upgraded, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the deleted settings file is back: %v", err)
	}
}

// Review of the hook installers: an edit made a 0644 settings file 0600.
func TestHookInstallKeepsTheSettingsFileMode(t *testing.T) {
	home := t.TempDir()
	path := SettingsPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallPermissionHookAt(home, path, "/opt/amq-remote"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("mode after install = %v, %v; want 0644", info.Mode().Perm(), err)
	}
}

// Review of the kept hook binary: whether a repair kept the installed
// binary depended on the process PATH, so a machine agent started without
// it replaced a working stable install.
func TestEnsureKeepsAnInstalledBinaryOffPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PATH", filepath.Join(home, "empty"))
	path := SettingsPath(home)
	installed, other := filepath.Join(home, "opt", "amq-remote"), filepath.Join(home, "bin", "amq-remote")
	for _, bin := range []string{installed, other} {
		if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(bin, nil, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := InstallPermissionHookAt(home, path, installed); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if err := os.WriteFile(path, bytes.Replace(raw, []byte(`claude permission-hook"`), []byte(`claude permission-hook --wait 600"`), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsurePermissionHook(home, path, other); err != nil {
		t.Fatal(err)
	}
	if cmd, _ := PermissionHookCommand(path); !strings.HasSuffix(cmd, "'"+installed+"' claude permission-hook") {
		t.Fatalf("repaired hook %q; want it to keep %s", cmd, installed)
	}
}
