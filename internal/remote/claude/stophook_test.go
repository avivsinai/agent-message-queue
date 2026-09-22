package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests run against a temp HOME only (architect ruling 10:59Z: the
// real ~/.claude is shared by every agent on this machine; no live install
// without the sponsor's word). No test in this package writes outside
// t.TempDir().

func readStops(t *testing.T, path string) []stopHookEntry {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var generic struct {
		Hooks map[string]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	var stops []stopHookEntry
	if rawStop, ok := generic.Hooks["Stop"]; ok {
		if err := json.Unmarshal(rawStop, &stops); err != nil {
			t.Fatal(err)
		}
	}
	return stops
}

func TestInstallStopHookIdempotentPreservesForeign(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude", "settings.json")
	// Pre-existing settings with a foreign Stop hook and unrelated keys.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	prior := `{
  "$schema": "https://json.schemastore.org/claude-code-settings.json",
  "model": "claude-fable-5-1[1m]",
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "bash enforce.sh", "timeout": 5}]}
    ],
    "Stop": [
      {"matcher": "other-tool", "hooks": [{"type": "command", "command": "echo foreign"}]}
    ]
  }
}`
	if err := os.WriteFile(path, []byte(prior), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := installStopHookAt(path); err != nil {
		t.Fatalf("install: %v", err)
	}
	stops := readStops(t, path)
	if len(stops) != 2 {
		t.Fatalf("Stop entries = %d, want 2 (ours + foreign)", len(stops))
	}
	foundOurs, foundForeign := false, false
	for _, e := range stops {
		if entryIsOurs(e) {
			foundOurs = true
		}
		if e.Matcher == "other-tool" && len(e.Hooks) == 1 && e.Hooks[0].Command == "echo foreign" {
			foundForeign = true
		}
	}
	if !foundOurs || !foundForeign {
		t.Fatalf("ours=%v foreign=%v, want both preserved", foundOurs, foundForeign)
	}
	// PreToolUse untouched.
	var generic2 struct {
		Hooks map[string]json.RawMessage `json:"hooks"`
	}
	raw, _ := os.ReadFile(path)
	_ = json.Unmarshal(raw, &generic2)
	var pre []stopHookEntry
	_ = json.Unmarshal(generic2.Hooks["PreToolUse"], &pre)
	if len(pre) != 1 || pre[0].Hooks[0].Command != "bash enforce.sh" {
		t.Fatalf("foreign PreToolUse hook damaged: %+v", pre)
	}
	// model key untouched.
	if !strings.Contains(string(raw), "claude-fable-5-1[1m]") {
		t.Fatal("unrelated top-level key lost during install")
	}

	// Idempotent: a second install is a no-op.
	if _, err := installStopHookAt(path); err != nil {
		t.Fatal(err)
	}
	if stops = readStops(t, path); len(stops) != 2 {
		t.Fatalf("second install produced %d Stop entries, want 2", len(stops))
	}
}

func TestUninstallRestoresForeignHooksOnlyRemovesOurs(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	prior := `{"hooks":{"Stop":[{"matcher":"other-tool","hooks":[{"type":"command","command":"echo foreign"}]}]}}`
	if err := os.WriteFile(path, []byte(prior), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := installStopHookAt(path); err != nil {
		t.Fatal(err)
	}
	removed, err := uninstallStopHookAt(path)
	if err != nil || !removed {
		t.Fatalf("uninstall: removed=%v err=%v", removed, err)
	}
	stops := readStops(t, path)
	if len(stops) != 1 || stops[0].Matcher != "other-tool" {
		t.Fatalf("foreign Stop entry not preserved: %+v", stops)
	}
	// Uninstall with nothing of ours present: no-op, not an error.
	removed, err = uninstallStopHookAt(path)
	if err != nil || removed {
		t.Fatalf("second uninstall: removed=%v err=%v, want false,nil", removed, err)
	}
}

func TestUninstallRemovesEmptyHooksObject(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude", "settings.json")
	if _, err := installStopHookAt(path); err != nil {
		t.Fatal(err)
	}
	if removed, err := uninstallStopHookAt(path); err != nil || !removed {
		t.Fatalf("uninstall: removed=%v err=%v", removed, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if _, has := doc["hooks"]; has {
		t.Fatalf("empty hooks object left behind: %s", raw)
	}
}

func TestRefusesToEditNonJSONSettings(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := installStopHookAt(path); err == nil {
		t.Fatal("installer edited a non-JSON settings document")
	}
	// The file is byte-unchanged.
	raw, _ := os.ReadFile(path)
	if string(raw) != "not json" {
		t.Fatal("non-JSON settings were modified")
	}
}

func TestRestoreByteForByte(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	prior := []byte(`{"a":1}`)
	if err := os.WriteFile(path, prior, 0o600); err != nil {
		t.Fatal(err)
	}
	raw, existed, err := readSettingsRaw(path)
	if err != nil || !existed {
		t.Fatalf("readSettingsRaw: %v %v", existed, err)
	}
	if _, err := installStopHookAt(path); err != nil {
		t.Fatal(err)
	}
	if err := restoreSettings(path, raw, existed); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(prior) {
		t.Fatalf("restore not byte-for-byte:\nprior  %s\nafter  %s", prior, after)
	}
	// Restore with existed=false removes a created file.
	home2 := t.TempDir()
	path2 := filepath.Join(home2, ".claude", "settings.json")
	if _, err := installStopHookAt(path2); err != nil {
		t.Fatal(err)
	}
	if err := restoreSettings(path2, nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path2); !os.IsNotExist(err) {
		t.Fatalf("created settings file survived a no-prior restore: %v", err)
	}
}
