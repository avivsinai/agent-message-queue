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
// shows a startup screen. Submit through the endpoint is busy while the
// seat's marker is open, and the identical resubmit runs once it closes.
// Review finding: the daemon socket was given as its resolved target path,
// outside CODEX_HOME, and the hold read the Codex home from the socket path
// and failed open.
func TestSubmitIsHeldWhileTheSeatStartupMarkerIsOpen(t *testing.T) {
	for _, tc := range []struct {
		name string
		// serveHome is the CODEX_HOME amq-remote itself runs with; "" is
		// the seat's own home.
		serveHome func(t *testing.T) string
	}{
		{name: "amq-remote shares the seat's Codex home"},
		{
			// Post-release review v0.113.2 finding R3: the hold looked the
			// thread up under amq-remote's own CODEX_HOME, so a serve process
			// started without the seat's home failed open and sent the turn.
			name:      "amq-remote runs under another Codex home",
			serveHome: func(t *testing.T) string { return t.TempDir() },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			codexHome := t.TempDir()
			serveHome := codexHome
			if tc.serveHome != nil {
				serveHome = tc.serveHome(t)
			}
			t.Setenv("CODEX_HOME", serveHome)
			sockDir, err := os.MkdirTemp("", "amqcx")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
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
			sock, srv := startFakeAppServerAt(t, filepath.Join(sockDir, "s.sock"))
			srv.setResumePath(filepath.Join(codexHome, "sessions", "2026", "10", "26", "rollout-2026-10-26T10-00-00-"+thread+".jsonl"))
			att, err := Attach(sock, thread)
			if err != nil {
				t.Fatalf("attach: %v", err)
			}
			t.Cleanup(func() { _ = att.Close() })
			<-srv.calls // initialize
			<-srv.calls // thread/resume
			store, err := requests.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			ep := core.New(core.Config{Store: store})
			ep.Register(att)
			t.Cleanup(func() { _ = ep.Close() })
			s := att.Inspect()
			id := "11111111-1111-4111-8111-111111111ae1"
			cmd := &protocol.Command{
				Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit,
				RequestID: id, TargetID: s.TargetID, Epoch: s.Epoch,
				NotAfter: protocol.FormatTime(time.Now().Add(time.Minute)),
				Input:    &protocol.SubmitInput{Text: "say PONG"},
			}
			local := core.Source{Host: "local"}
			out, err := ep.Handle(cmd, local)
			if err != nil || out.(protocol.Reply).Outcome.Code != protocol.CodeBusy {
				t.Fatalf("submit during the seat's startup screens = %+v, %v; want busy", out, err)
			}
			select {
			case call := <-srv.calls:
				t.Fatalf("submit sent %s to the daemon while the seat's marker was open", call.Method)
			default:
			}

			// The seat's dialog hook closes the marker; the identical resubmit runs.
			if err := os.Remove(marker); err != nil {
				t.Fatal(err)
			}
			key := requests.Key{CreatorHost: "local", TargetID: s.TargetID, RequestID: id}
			srv.setTurnStartDelay(func() {
				srv.notify(t, "turn/started", `{"threadId":"`+thread+`","turn":{"id":"turnA"}}`)
				srv.notify(t, "item/started", `{"threadId":"`+thread+`","turnId":"turnA","item":{"type":"userMessage","id":"iA","clientId":"`+clientIDFor(key)+`","content":[]}}`)
			})
			done := make(chan protocol.Reply, 1)
			go func() {
				out, _ := ep.Handle(cmd, local)
				reply, _ := out.(protocol.Reply)
				done <- reply
			}()
			select {
			case call := <-srv.calls:
				if call.Method != "turn/start" {
					t.Fatalf("resubmit after the marker closed sent %s, want turn/start", call.Method)
				}
			case reply := <-done:
				t.Fatalf("resubmit after the marker closed = %s %q without a turn/start", reply.Snapshot.State, reply.Outcome.Code)
			case <-time.After(10 * time.Second):
				t.Fatal("resubmit after the marker closed sent nothing")
			}
			select {
			case reply := <-done:
				if reply.Snapshot.State != protocol.StateRunning || reply.Outcome.Code != "" {
					t.Fatalf("resubmit after the marker closed = %s %q, want running", reply.Snapshot.State, reply.Outcome.Code)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("resubmit after the marker closed did not return")
			}
		})
	}
}
