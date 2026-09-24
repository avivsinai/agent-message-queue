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
	// A well-formed payload for a session that is not attached or bound
	// still exits 0 and writes nothing (bead 611.37).
	payload, _ := json.Marshal(StopHookPayload{SessionID: "sess-abc", HookEventName: "Stop"})
	if code := RunStopHookReceiver(home, strings.NewReader(string(payload)), os.Stderr); code != 0 {
		t.Fatalf("unbound payload exit = %d, want 0", code)
	}
	if _, err := os.Stat(stopMarkerPath(home, "sess-abc")); !os.IsNotExist(err) {
		t.Fatal("unbound session wrote a marker")
	}
}

// TestStopHookBoundSessionStaysSmall is the 611.37 happy path: an unbound
// turn writes nothing, and a bound session's marker stays small across
// many turns and is removed when the attachment detaches.
func TestStopHookBoundSessionStaysSmall(t *testing.T) {
	if !noFollowSupported {
		t.Skip("stop hook receiver needs a no-follow open")
	}
	const sid = "sess-bound"
	home := tempHome(t, 7, &sessionRegistry{Pid: 7, SessionID: sid, Kind: "interactive"})
	if code := RunStopHookReceiver(home, strings.NewReader(`{"session_id":"sess-other","hook_event_name":"Stop"}`), os.Stderr); code != 0 {
		t.Fatalf("unbound exit = %d, want 0", code)
	}
	if _, err := os.Stat(stopMarkerPath(home, "sess-other")); !os.IsNotExist(err) {
		t.Fatal("unbound session wrote a marker")
	}
	att, err := Attach(config{Pid: 7, Home: home, Target: "cc-bound"})
	if err != nil {
		t.Fatal(err)
	}
	payload := `{"session_id":"` + sid + `","hook_event_name":"Stop"}`
	for range 300 {
		if code := RunStopHookReceiver(home, strings.NewReader(payload), os.Stderr); code != 0 {
			t.Fatalf("bound exit = %d, want 0", code)
		}
	}
	marker := stopMarkerPath(home, sid)
	fi, err := os.Stat(marker)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() == 0 {
		t.Fatal("bound session wrote an empty marker")
	}
	raw, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), sid) {
		t.Fatalf("marker missing session: %q", raw)
	}
	att.Subscribe(nil)()
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("detach left the marker file")
	}
}
