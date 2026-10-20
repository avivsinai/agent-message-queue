package codex

import (
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// 611.42 slice 1b: an approval of a turn the user started in the Codex TUI
// (no run of ours, so not in byTurn) was dropped. It now becomes a thread
// approval shown on the owner share, and the owner's approve reaches the
// app-server as the decision.
func TestTUITurnApprovalAnsweredFromOwnerShare(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	att, err := Attach(sock, "t1", WithApprovals(true))
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
	ep := core.New(core.Config{Store: store})
	t.Cleanup(func() { _ = ep.Close() })
	ep.Register(att)
	owner := core.Source{Host: "buzz-1", Origin: map[string]string{"carrier": "buzz", "body": "b1", "channel": "dm-1"}}
	shown := make(chan core.ThreadApproval, 4)
	ep.SetThreadApprovals(core.ThreadApprovalSink{
		Owner: func(target, native string) (core.Source, bool) { return owner, native == "t1" },
		Publish: func(ta core.ThreadApproval) error {
			shown <- ta
			return nil
		},
	})
	next := func() core.ThreadApproval {
		t.Helper()
		select {
		case ta := <-shown:
			return ta
		case <-time.After(5 * time.Second):
			t.Fatal("thread approval never published")
		}
		return core.ThreadApproval{}
	}

	srv.notify(t, "turn/started", `{"threadId":"t1","turn":{"id":"tui-1"}}`)
	srv.sendServerRequest(t, "7", "item/commandExecution/requestApproval", `{"threadId":"t1","turnId":"tui-1","itemId":"c1","command":"go test ./..."}`)
	ta := next()
	if ta.Turn != "tui-1" || ta.NativeSession != "t1" || ta.Interaction.ApproveOption != "accept" || ta.Origin["body"] != "b1" {
		t.Fatalf("published %+v, want the TUI turn's approval with approve offered on the owner share", ta)
	}

	if err := ep.RespondThreadApproval(owner, ta.ID, "accept"); err != nil {
		t.Fatalf("respond: %v", err)
	}
	for answered := false; !answered; {
		select {
		case msg := <-srv.calls:
			if msg.Method == "" && msg.ID != nil && string(*msg.ID) == `"7"` {
				if string(msg.Result) != `{"decision":"accept"}` {
					t.Fatalf("response = %s, want accept", msg.Result)
				}
				answered = true
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the decision never reached the app-server")
		}
	}
	if done := next(); done.Resolution == nil || done.Resolution.Outcome != protocol.ResolutionAnswered || done.Resolution.Option != "accept" {
		t.Fatalf("resolution = %+v, want answered with accept", done.Resolution)
	}
}
