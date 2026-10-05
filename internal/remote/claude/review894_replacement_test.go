package claude

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReview894ReplacementKeepsStopScope(t *testing.T) {
	const sid = "review-replacement"
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
	RunStopHookReceiver(home, strings.NewReader(`{"session_id":"review-replacement","hook_event_name":"Stop"}`), io.Discard)
	if _, err := os.Stat(stopMarkerPath(home, sid)); err != nil {
		t.Fatalf("replacement lost Stop scope: %v", err)
	}
}

func TestReview894RotationCannotHideFreshStop(t *testing.T) {
	const sid = "review-rotation"
	home := t.TempDir()
	t.Setenv("AMQ_REMOTE_BINDING", filepath.Join(home, "absent-binding.json"))
	if _, err := bindStopSession(home, sid); err != nil {
		t.Fatal(err)
	}
	write := func() {
		RunStopHookReceiver(home, strings.NewReader(`{"session_id":"review-rotation","hook_event_name":"Stop"}`), io.Discard)
	}
	write()
	path := stopMarkerPath(home, sid)
	first, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Later turns append. The reader at the old offset still sees them.
	write()
	got := readStopMarkers(path, first.Size())
	if len(got.lines) == 0 {
		t.Fatal("later Stop is invisible at the prior consumed offset")
	}
}

func TestReview894MailboxBindingIgnoresStaleSentinel(t *testing.T) {
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

// Bead agent-message-queue-7wq (Ben review F8): a leftover legacy
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

// Bead agent-message-queue-7wq, Pro review of #952: an odd bindings entry is
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
