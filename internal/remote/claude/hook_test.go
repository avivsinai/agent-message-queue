package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

// TestStopHookBoundSessionAppendsUntilUnsubscribe is the 611.37 happy
// path: a bound session's turns append lines the confirmation poller reads,
// a later turn stays visible at the prior consumed offset (#894), and the
// marker is removed when the attachment unsubscribes.
func TestStopHookBoundSessionAppendsUntilUnsubscribe(t *testing.T) {
	if !noFollowSupported {
		t.Skip("stop hook receiver needs a no-follow open")
	}
	const sid = "sess-bound"
	home := tempHome(t, 7, &sessionRegistry{Pid: 7, SessionID: sid, Kind: "interactive"})
	t.Setenv("AMQ_REMOTE_BINDING", filepath.Join(home, "absent-binding.json"))
	att, err := Attach(config{Pid: 7, Home: home, Target: "cc-bound"})
	if err != nil {
		t.Fatal(err)
	}
	write := func() {
		if code := RunStopHookReceiver(home, strings.NewReader(`{"session_id":"`+sid+`","hook_event_name":"Stop"}`), os.Stderr); code != 0 {
			t.Fatalf("bound exit = %d, want 0", code)
		}
	}
	write()
	marker := stopMarkerPath(home, sid)
	first, err := os.Stat(marker)
	if err != nil {
		t.Fatal(err)
	}
	write()
	if got := readStopMarkers(marker, first.Size()); len(got.lines) != 1 || got.lines[0].ts <= 0 {
		t.Fatalf("markers after the first turn = %+v, want one timed line", got)
	}
	att.Subscribe(nil)()
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("unsubscribe left the marker file")
	}
}
