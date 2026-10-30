package claude

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUninstallWithoutMarkedEntryLeavesFileUntouched(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(SettingsPath(home)), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("{\"a\":1}\n")
	if err := os.WriteFile(SettingsPath(home), original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := UninstallStopHook(home); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(SettingsPath(home))
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
	// still exits 0 and writes nothing.
	payload, _ := json.Marshal(StopHookPayload{SessionID: "sess-abc", HookEventName: "Stop"})
	if code := RunStopHookReceiver(home, strings.NewReader(string(payload)), os.Stderr); code != 0 {
		t.Fatalf("unbound payload exit = %d, want 0", code)
	}
	if _, err := os.Stat(stopMarkerPath(home, "sess-abc")); !os.IsNotExist(err) {
		t.Fatal("unbound session wrote a marker")
	}
}

// TestStopHookBoundSessionAppendsUntilUnsubscribe is the happy
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

func TestPinWarningsNamesAutoMode(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(SettingsPath(home)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SettingsPath(home), []byte(`{"permissions":{"defaultMode":"auto"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	warns, err := PinWarnings(SettingsPath(home))
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "auto") {
		t.Fatalf("warnings = %q, want one naming auto mode", warns)
	}
}

// Regression: #855, the installed hook is a matcher group whose
// nested command runs the quoted amq-remote binary (the marker is an env
// assignment in front of it, not the command word).
func TestInstalledStopHookShapeAndCommandWord(t *testing.T) {
	home, installed := installSettings(t, "")
	var s settingsShape
	if err := json.Unmarshal([]byte(installed), &s); err != nil {
		t.Fatalf("installed settings is not valid JSON: %v\n%s", err, installed)
	}
	groups := s.Hooks["Stop"]
	if len(groups) != 1 || len(groups[0].Hooks) != 1 {
		t.Fatalf("Stop chain = %+v, want one group with one hook", groups)
	}
	cmd := groups[0].Hooks[0].Command
	if !strings.HasPrefix(cmd, stopHookMarker) || !strings.HasSuffix(cmd, ` || exec '/opt/amq bin/amq-remote' claude stop-hook`) {
		t.Fatalf("command = %q, want the marker first and the quoted binary exec'd last", cmd)
	}
	// A second install of the same hook is a no-op.
	if err := InstallStopHook(home, "/opt/amq bin/amq-remote"); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(SettingsPath(home)); string(again) != installed {
		t.Fatalf("second install rewrote settings.json:\n%s", again)
	}
	// A reinstall from a new binary kept the stop hook on an old worktree
	// build while it printed "stop hook installed".
	if err := InstallStopHook(home, "/opt/homebrew/bin/amq-remote"); err != nil {
		t.Fatal(err)
	}
	moved, _ := os.ReadFile(SettingsPath(home))
	if err := json.Unmarshal(moved, &s); err != nil {
		t.Fatal(err)
	}
	if groups := s.Hooks["Stop"]; len(groups) != 1 || len(groups[0].Hooks) != 1 ||
		groups[0].Hooks[0].Command != stopHookCommand(home, "/opt/homebrew/bin/amq-remote") {
		t.Fatalf("reinstall from another binary = %+v, want one hook on the new binary", groups)
	}
}

// Regression: #855, install into existing hooks without
// Stop, into an operator's existing empty Stop array, and in front of an
// operator's own Stop hook. Install yields valid JSON with one hooks object
// and the prior hooks intact. Uninstall removes our hook and never an
// operator container: a file with its own Stop array round-trips byte for
// byte, including a 2^53 number a float decode would corrupt; the other
// keeps the Stop array we had to create, empty.
func TestInstallIntoExistingHooksRoundTrips(t *testing.T) {
	operatorStop := "{\n  \"hooks\": {\n    \"Stop\": [\n      {\n        \"type\": \"command\",\n        \"command\": \"echo unrelated\"\n      }\n    ]\n  },\n  \"precision_fixture\": 9007199254740993\n}\n"
	for name, tc := range map[string]struct {
		original, afterUninstall string
		stopGroups               int
	}{
		"hooks without Stop": {
			original:       "{\n  \"hooks\": {\n    \"PreToolUse\": [{\"matcher\": \"*\", \"hooks\": [{\"type\": \"command\", \"command\": \"echo pre\"}]}]\n  }\n}\n",
			afterUninstall: "{\n  \"hooks\": {\"Stop\":[],\n    \"PreToolUse\": [{\"matcher\": \"*\", \"hooks\": [{\"type\": \"command\", \"command\": \"echo pre\"}]}]\n  }\n}\n",
			stopGroups:     1,
		},
		"operator's empty Stop": {
			original:       "{\"hooks\":{\"Stop\":[]},\"x\":1}\n",
			afterUninstall: "{\"hooks\":{\"Stop\":[]},\"x\":1}\n",
			stopGroups:     1,
		},
		"operator's Stop hook": {original: operatorStop, afterUninstall: operatorStop, stopGroups: 2},
	} {
		t.Run(name, func(t *testing.T) {
			home, installed := installSettings(t, tc.original)
			if !json.Valid([]byte(installed)) {
				t.Fatalf("install produced invalid JSON:\n%s", installed)
			}
			if n := strings.Count(installed, `"hooks": {`) + strings.Count(installed, `"hooks":{`); n != 1 {
				t.Fatalf("install produced %d top-level hooks objects:\n%s", n, installed)
			}
			var s settingsShape
			_ = json.Unmarshal([]byte(installed), &s)
			if len(s.Hooks["Stop"]) != tc.stopGroups {
				t.Fatalf("Stop chain has %d groups, want %d:\n%s", len(s.Hooks["Stop"]), tc.stopGroups, installed)
			}
			if strings.Contains(tc.original, "PreToolUse") && len(s.Hooks["PreToolUse"]) != 1 {
				t.Fatalf("prior PreToolUse hooks lost:\n%s", installed)
			}
			if err := UninstallStopHook(home); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(SettingsPath(home))
			if string(got) != tc.afterUninstall {
				t.Fatalf("after uninstall:\nwant %q\ngot  %q", tc.afterUninstall, got)
			}
		})
	}
}

// Regression: #855, an operator added a foreign hook to the matcher
// group we installed. Uninstall removes our inner hook and keeps theirs.
func TestUninstallFromMixedGroupKeepsForeignHook(t *testing.T) {
	home, _ := installSettings(t, "")
	raw, _ := os.ReadFile(SettingsPath(home))
	mixed := strings.Replace(string(raw), `,"timeout":5}]`, `,"timeout":5},{"type":"command","command":"echo operator"}]`, 1)
	if mixed == string(raw) {
		t.Fatalf("setup: could not add a foreign hook to:\n%s", raw)
	}
	if err := os.WriteFile(SettingsPath(home), []byte(mixed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := UninstallStopHook(home); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(SettingsPath(home))
	var s settingsShape
	if err := json.Unmarshal(got, &s); err != nil {
		t.Fatalf("uninstall produced invalid JSON: %v\n%s", err, got)
	}
	if strings.Contains(string(got), stopHookMarker) {
		t.Fatalf("our hook is still installed:\n%s", got)
	}
	if g := s.Hooks["Stop"]; len(g) != 1 || len(g[0].Hooks) != 1 || g[0].Hooks[0].Command != "echo operator" {
		t.Fatalf("foreign hook not preserved: %+v\n%s", g, got)
	}
}

// Regression: #855, a session id with separators must not
// become a path outside the marker directory, even when a binding names it.
func TestReceiverRefusesTraversalSessionID(t *testing.T) {
	home := t.TempDir()
	bindingPath := filepath.Join(home, "binding.json")
	if err := os.WriteFile(bindingPath, []byte(`{"native_session":"../escape"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMQ_REMOTE_BINDING", bindingPath)
	if code := RunStopHookReceiver(home, strings.NewReader(`{"session_id":"../escape"}`), os.Stderr); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if _, err := os.Stat(filepath.Join(markerDir(home), "escape.jsonl")); err == nil {
		t.Fatal("receiver wrote outside the amq-stop directory")
	}
}

// Regression: #894, a replacement attachment keeps the stop-marker scope.
func TestReplacementKeepsStopScope(t *testing.T) {
	const sid = "replacement-session"
	home := tempHome(t, 7, &sessionRegistry{Pid: 7, SessionID: sid, Kind: "interactive"})
	t.Setenv("AMQ_REMOTE_BINDING", filepath.Join(home, "absent-binding.json"))
	old, err := Attach(config{Pid: 7, Home: home, Target: "cc-bound"})
	if err != nil {
		t.Fatal(err)
	}
	stopOld := old.Subscribe(nil)
	next, err := Attach(config{Pid: 7, Home: home, Target: "cc-bound"})
	if err != nil {
		t.Fatal(err)
	}
	// Endpoint.Register receives an already-created replacement, then
	// unsubscribes the old attachment before subscribing the new one.
	stopOld()
	defer next.Subscribe(nil)()
	next.trackBoundSession(sid)
	RunStopHookReceiver(home, strings.NewReader(`{"session_id":"replacement-session","hook_event_name":"Stop"}`), io.Discard)
	if _, err := os.Stat(stopMarkerPath(home, sid)); err != nil {
		t.Fatalf("replacement lost Stop scope: %v", err)
	}
}

func TestMailboxBindingIgnoresStaleSentinel(t *testing.T) {
	if !noFollowSupported {
		t.Skip("stop hook receiver needs a no-follow open")
	}
	const sid = "stale-session"
	home := t.TempDir()
	path := filepath.Join(home, "binding.json")
	if err := os.WriteFile(path, []byte("{\"root\":\"/r\",\"target\":\"t\",\"native_session\":\"\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMQ_REMOTE_BINDING", path)
	if _, err := bindStopSession(home, sid); err != nil {
		t.Fatal(err)
	}
	RunStopHookReceiver(home, strings.NewReader(`{"session_id":"stale-session","hook_event_name":"Stop"}`), io.Discard)
	if _, err := os.Stat(stopMarkerPath(home, sid)); !os.IsNotExist(err) {
		t.Fatal("a mailbox binding wrote a marker from a stale sentinel")
	}
}

// Regression: agent-message-queue-7wq, a leftover legacy
// binding.json for another session must not deny a session bound under
// bindings/.
func TestStopHookAllowsSessionBoundUnderBindingsDir(t *testing.T) {
	if !noFollowSupported {
		t.Skip("stop hook receiver needs a no-follow open")
	}
	const sid = "session-y"
	home := t.TempDir()
	path := filepath.Join(home, "binding.json")
	if err := os.WriteFile(path, []byte("{\"root\":\"/r\",\"target\":\"t\",\"native_session\":\"session-x\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, "bindings"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "bindings", "y.json"), []byte("{\"root\":\"/r\",\"target\":\"t\",\"native_session\":\"session-y\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMQ_REMOTE_BINDING", path)
	RunStopHookReceiver(home, strings.NewReader(`{"session_id":"session-y","hook_event_name":"Stop"}`), io.Discard)
	if _, err := os.Stat(stopMarkerPath(home, sid)); err != nil {
		t.Fatalf("session bound under bindings/ was denied: %v", err)
	}
}

// Regression: #952, an odd bindings entry is
// binding presence and is never followed, so a stale sentinel cannot write.
func TestStopHookDeniesOddBindingsEntries(t *testing.T) {
	if !noFollowSupported {
		t.Skip("stop hook receiver needs a no-follow open")
	}
	const sid = "session-y"
	match := []byte("{\"root\":\"/r\",\"target\":\"t\",\"native_session\":\"session-y\"}\n")
	other := []byte("{\"root\":\"/r\",\"target\":\"t\",\"native_session\":\"session-x\"}\n")
	rows := map[string]func(t *testing.T, home string){
		"symlinked bindings dir to a match": func(t *testing.T, home string) {
			real := filepath.Join(t.TempDir(), "real")
			if err := os.Mkdir(real, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(real, "y.json"), match, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, "binding.json"), other, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, filepath.Join(home, "bindings")); err != nil {
				t.Fatal(err)
			}
		},
		"dangling bindings symlink": func(t *testing.T, home string) {
			if err := os.Symlink(filepath.Join(home, "gone"), filepath.Join(home, "bindings")); err != nil {
				t.Fatal(err)
			}
		},
		"bindings entry is a directory": func(t *testing.T, home string) {
			if err := os.MkdirAll(filepath.Join(home, "bindings", "y.json"), 0o700); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, setup := range rows {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("AMQ_REMOTE_BINDING", filepath.Join(home, "binding.json"))
			setup(t, home)
			if _, err := bindStopSession(home, sid); err != nil {
				t.Fatal(err)
			}
			RunStopHookReceiver(home, strings.NewReader(`{"session_id":"session-y","hook_event_name":"Stop"}`), io.Discard)
			if _, err := os.Stat(stopMarkerPath(home, sid)); !os.IsNotExist(err) {
				t.Fatal("an odd bindings entry let a stale sentinel write")
			}
		})
	}
}

// Regression: detach and prune leave <name>.ended.json, and the hook
// read it as a binding naming the session, so a detached session that kept
// running appended a Stop marker on every turn. A tombstone is not binding
// evidence.
func TestStopHookIgnoresATombstoneNamingTheSession(t *testing.T) {
	if !noFollowSupported {
		t.Skip("stop hook receiver needs a no-follow open")
	}
	const sid = "session-y"
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, "bindings"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "bindings", "y.ended.json"), []byte("{\"root\":\"/r\",\"target\":\"t\",\"native_session\":\"session-y\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMQ_REMOTE_BINDING", filepath.Join(home, "binding.json"))
	if got := bindingDecision(home, sid); got != bindingAbsent {
		t.Fatalf("binding decision with only a tombstone = %d; want bindingAbsent", got)
	}
	RunStopHookReceiver(home, strings.NewReader(`{"session_id":"session-y","hook_event_name":"Stop"}`), io.Discard)
	if _, err := os.Stat(stopMarkerPath(home, sid)); !os.IsNotExist(err) {
		t.Fatalf("a detached session's tombstone wrote a Stop marker: %v", err)
	}
}
