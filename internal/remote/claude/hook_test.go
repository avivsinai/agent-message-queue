package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallStopHookIdempotentAndMarked(t *testing.T) {
	home := t.TempDir()
	if err := InstallStopHook(home, "/usr/local/bin/amq-remote"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(settingsPath(home))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), stopHookMarker) {
		t.Fatal("installed hook command lacks the ownership marker")
	}
	before := string(raw)
	// Second install is a no-op (idempotent).
	if err := InstallStopHook(home, "/usr/local/bin/amq-remote"); err != nil {
		t.Fatal(err)
	}
	raw2, _ := os.ReadFile(settingsPath(home))
	if string(raw2) != before {
		t.Fatal("second install rewrote settings.json — install must be idempotent")
	}
}

// TestUninstallByteForByteWithBillionFixture pins the uninstall contract
// on the 2^53 fixture: a settings file whose unrelated number is
// 9007199254740993 must round-trip EXACTLY through install+uninstall — a
// float64 decode would corrupt it to 9007199254740992 and the file would
// drift even though the hook entry was removed cleanly.
func TestUninstallByteForByteWithBillionFixture(t *testing.T) {
	home := t.TempDir()
	original := "{\n  \"hooks\": {\n    \"Stop\": [\n      {\n        \"type\": \"command\",\n        \"command\": \"echo unrelated\"\n      }\n    ]\n  },\n  \"precision_fixture\": 9007199254740993,\n  \"nested\": {\"n\": 9007199254740993}\n}\n"
	dir := filepath.Dir(settingsPath(home))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPath(home), []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := InstallStopHook(home, "amq-remote"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(settingsPath(home))
	if !strings.Contains(string(after), "9007199254740993") {
		t.Fatal("install corrupted the 2^53 fixture — json.Number preservation failed")
	}
	if err := UninstallStopHook(home); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(settingsPath(home))
	if string(got) != original {
		t.Fatalf("uninstall is not byte-for-byte:\nwant %q\ngot  %q", original, string(got))
	}
}

func TestUninstallWithoutMarkedEntryLeavesFileUntouched(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(settingsPath(home)), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("{\"a\":1}\n")
	if err := os.WriteFile(settingsPath(home), original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := UninstallStopHook(home); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(settingsPath(home))
	if string(got) != string(original) {
		t.Fatal("uninstall with no marked entry rewrote the file")
	}
}

func TestInstallRefusesNonRegularSettingsLeaf(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(settingsPath(home)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(settingsPath(home), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := InstallStopHook(home, "amq-remote"); err == nil {
		t.Fatal("install accepted a directory at the settings leaf")
	}
}

func TestReceiverFailOpen(t *testing.T) {
	home := t.TempDir()
	// Garbage stdin, missing session id, whatever: always exit 0, never
	// an error surface (exit 2 would block Claude Code).
	if code := RunStopHookReceiver(home, strings.NewReader("not json"), os.Stderr); code != 0 {
		t.Fatalf("garbage stdin exit = %d, want 0 (fail-open)", code)
	}
	if code := RunStopHookReceiver(home, strings.NewReader(`{"hook_event_name":"Stop"}`), os.Stderr); code != 0 {
		t.Fatalf("missing session exit = %d, want 0 (fail-open)", code)
	}
	// A well-formed payload appends the marker.
	payload, _ := json.Marshal(StopHookPayload{SessionID: "sess-abc", HookEventName: "Stop"})
	if code := RunStopHookReceiver(home, strings.NewReader(string(payload)), os.Stderr); code != 0 {
		t.Fatalf("good payload exit = %d, want 0", code)
	}
	raw, err := os.ReadFile(stopMarkerPath(home, "sess-abc"))
	if err != nil {
		t.Fatalf("marker not written: %v", err)
	}
	if !strings.Contains(string(raw), `"session_id":"sess-abc"`) {
		t.Fatalf("marker line wrong: %q", raw)
	}
}
