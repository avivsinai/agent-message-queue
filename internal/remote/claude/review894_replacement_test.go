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
