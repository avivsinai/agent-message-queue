package codex

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/codexidentity"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// Bead agent-message-queue-uue (lead ruling on the codex-cli 0.164.0 live
// check): the first-prompt hook close releases a daemon seat's Startup
// marker, and a turn amq-remote submits on the seat's thread runs that hook
// as well, so a remote prompt could end the hold while the seat's TUI still
// shows a startup screen. Submit is held while the seat's marker is open.
func TestSubmitIsHeldWhileTheSeatStartupMarkerIsOpen(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	codexHome, err := os.MkdirTemp("", "amqcx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(codexHome) })
	if err := os.Mkdir(filepath.Join(codexHome, "c"), 0o700); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := fsq.EnsureAgentDirs(root, "cx"); err != nil {
		t.Fatal(err)
	}
	const thread = "019a0000-0000-7000-8000-000000000001"
	if err := codexidentity.Publish(codexidentity.Record{Thread: thread, CodexHome: codexHome, Root: root, BaseRoot: root, Me: "cx", RootID: "r1", BaseRootID: "r1"}); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(fsq.AgentBase(root, "cx"), seatDialogMarker)
	raw := `{"schema":1,"event":"Startup","harness":"codex","kind":"hook_review","cwd":"/work","opened_at":"x"}` + "\n"
	if err := os.WriteFile(marker, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	sock, srv := startFakeAppServerAt(t, filepath.Join(codexHome, "c", "s.sock"))
	att, err := Attach(sock, thread)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })
	<-srv.calls // initialize
	<-srv.calls // thread/resume
	s := att.Inspect()
	key := requests.Key{CreatorHost: "local", TargetID: s.TargetID, RequestID: "11111111-1111-4111-8111-111111111uue"}
	adm, err := att.Submit(core.BoundRequest{Key: key, Epoch: s.Epoch, Input: protocol.SubmitInput{Text: "say PONG"}})
	if err != nil || adm.Admitted || adm.Code != protocol.CodeBusy {
		t.Fatalf("submit during the seat's startup screens = %+v, %v; want busy", adm, err)
	}
	select {
	case call := <-srv.calls:
		t.Fatalf("submit sent %s to the daemon while the seat's marker was open", call.Method)
	default:
	}

	// The seat's dialog hook closes the marker; the next submit is sent.
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	srv.setTurnStartDelay(func() {
		srv.notify(t, "turn/started", `{"threadId":"`+thread+`","turn":{"id":"turnA"}}`)
		srv.notify(t, "item/started", `{"threadId":"`+thread+`","turnId":"turnA","item":{"type":"userMessage","id":"iA","clientId":"`+clientIDFor(key)+`","content":[]}}`)
	})
	done := make(chan core.Admission, 1)
	go func() {
		adm, _ := att.Submit(core.BoundRequest{Key: key, Epoch: s.Epoch, Input: protocol.SubmitInput{Text: "say PONG"}})
		done <- adm
	}()
	if call := <-srv.calls; call.Method != "turn/start" {
		t.Fatalf("submit after the marker closed sent %s, want turn/start", call.Method)
	}
	select {
	case adm := <-done:
		if !adm.Admitted {
			t.Fatalf("submit after the marker closed = %+v, want admitted", adm)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("submit after the marker closed did not return")
	}
}
